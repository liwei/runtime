// Copyright (c) 2018 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/kata-containers/runtime/virtcontainers/pkg/firecracker/client"
	models "github.com/kata-containers/runtime/virtcontainers/pkg/firecracker/client/models"
	ops "github.com/kata-containers/runtime/virtcontainers/pkg/firecracker/client/operations"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/kata-containers/runtime/virtcontainers/device/config"
	"github.com/kata-containers/runtime/virtcontainers/store"
	"github.com/kata-containers/runtime/virtcontainers/types"
	"github.com/kata-containers/runtime/virtcontainers/utils"
)

type vmmState uint8

const (
	notReady vmmState = iota
	apiReady
	vmReady
)

const (
	//fcTimeout is the maximum amount of time in seconds to wait for the VMM to respond
	fcTimeout            = 10
	fireSocket           = "firecracker.sock"
	fcStopSandboxTimeout = 15
	// This indicates the number of block devices that can be attached to the
	// firecracker guest VM.
	// We attach a pool of placeholder drives before the guest has started, and then
	// patch the replace placeholder drives with drives with actual contents.
	fcDiskPoolSize = 8
)

var fcKernelParams = append(commonVirtioblkKernelRootParams, []Param{
	// The boot source is the first partition of the first block device added
	{"pci", "off"},
	{"reboot", "k"},
	{"panic", "1"},
	{"iommu", "off"},
	{"8250.nr_uarts", "0"},
	{"net.ifnames", "0"},
	{"random.trust_cpu", "on"},

	// Firecracker doesn't support ACPI
	// Fix kernel error "ACPI BIOS Error (bug)"
	{"acpi", "off"},
}...)

func (s vmmState) String() string {
	switch s {
	case notReady:
		return "FC not ready"
	case apiReady:
		return "FC API ready"
	case vmReady:
		return "FC VM ready"
	}

	return ""
}

// FirecrackerInfo contains information related to the hypervisor that we
// want to store on disk
type FirecrackerInfo struct {
	PID int
}

type firecrackerState struct {
	sync.RWMutex
	state vmmState
}

func (s *firecrackerState) set(state vmmState) {
	s.Lock()
	defer s.Unlock()

	s.state = state
}

// firecracker is an Hypervisor interface implementation for the firecracker hypervisor.
type firecracker struct {
	id    string //Unique ID per pod. Normally maps to the sandbox id
	state firecrackerState
	info  FirecrackerInfo

	firecrackerd *exec.Cmd           //Tracks the firecracker process itself
	fcClient     *client.Firecracker //Tracks the current active connection
	socketPath   string

	store          *store.VCStore
	config         HypervisorConfig
	pendingDevices []firecrackerDevice // Devices to be added when the FC API is ready
	ctx            context.Context
}

type firecrackerDevice struct {
	dev     interface{}
	devType deviceType
}

// Logger returns a logrus logger appropriate for logging firecracker  messages
func (fc *firecracker) Logger() *logrus.Entry {
	return virtLog.WithField("subsystem", "firecracker")
}

func (fc *firecracker) trace(name string) (opentracing.Span, context.Context) {
	if fc.ctx == nil {
		fc.Logger().WithField("type", "bug").Error("trace called before context set")
		fc.ctx = context.Background()
	}

	span, ctx := opentracing.StartSpanFromContext(fc.ctx, name)

	span.SetTag("subsystem", "hypervisor")
	span.SetTag("type", "firecracker")

	return span, ctx
}

// For firecracker this call only sets the internal structure up.
// The sandbox will be created and started through startSandbox().
func (fc *firecracker) createSandbox(ctx context.Context, id string, hypervisorConfig *HypervisorConfig, vcStore *store.VCStore) error {
	fc.ctx = ctx

	span, _ := fc.trace("createSandbox")
	defer span.Finish()

	//TODO: check validity of the hypervisor config provided
	//https://github.com/kata-containers/runtime/issues/1065
	fc.id = id
	fc.socketPath = filepath.Join(store.SandboxRuntimeRootPath(fc.id), fireSocket)
	fc.store = vcStore
	fc.config = *hypervisorConfig
	fc.state.set(notReady)

	// No need to return an error from there since there might be nothing
	// to fetch if this is the first time the hypervisor is created.
	if err := fc.store.Load(store.Hypervisor, &fc.info); err != nil {
		fc.Logger().WithField("function", "init").WithError(err).Info("No info could be fetched")
	}

	return nil
}

func (fc *firecracker) newFireClient() *client.Firecracker {
	span, _ := fc.trace("newFireClient")
	defer span.Finish()
	httpClient := client.NewHTTPClient(strfmt.NewFormats())

	socketTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, path string) (net.Conn, error) {
			addr, err := net.ResolveUnixAddr("unix", fc.socketPath)
			if err != nil {
				return nil, err
			}

			return net.DialUnix("unix", nil, addr)
		},
	}

	transport := httptransport.New(client.DefaultHost, client.DefaultBasePath, client.DefaultSchemes)
	transport.Transport = socketTransport
	httpClient.SetTransport(transport)

	return httpClient
}

func (fc *firecracker) vmRunning() bool {
	resp, err := fc.client().Operations.DescribeInstance(nil)
	if err != nil {
		return false
	}

	// Be explicit
	switch *resp.Payload.State {
	case models.InstanceInfoStateStarting:
		// Unsure what we should do here
		fc.Logger().WithField("unexpected-state", models.InstanceInfoStateStarting).Debug("vmRunning")
		return false
	case models.InstanceInfoStateRunning:
		return true
	case models.InstanceInfoStateUninitialized, models.InstanceInfoStateHalting, models.InstanceInfoStateHalted:
		return false
	default:
		return false
	}
}

// waitVMM will wait for timeout seconds for the VMM to be up and running.
// This does not mean that the VM is up and running. It only indicates that the VMM is up and
// running and able to handle commands to setup and launch a VM
func (fc *firecracker) waitVMM(timeout int) error {
	span, _ := fc.trace("waitVMM")
	defer span.Finish()

	if timeout < 0 {
		return fmt.Errorf("Invalid timeout %ds", timeout)
	}

	timeStart := time.Now()
	for {
		_, err := fc.client().Operations.DescribeInstance(nil)
		if err == nil {
			return nil
		}

		if int(time.Since(timeStart).Seconds()) > timeout {
			return fmt.Errorf("Failed to connect to firecrackerinstance (timeout %ds): %v", timeout, err)
		}

		time.Sleep(time.Duration(10) * time.Millisecond)
	}
}

func (fc *firecracker) fcInit(timeout int) error {
	span, _ := fc.trace("fcInit")
	defer span.Finish()

	args := []string{"--api-sock", fc.socketPath}

	cmd := exec.Command(fc.config.HypervisorPath, args...)
	if err := cmd.Start(); err != nil {
		fc.Logger().WithField("Error starting firecracker", err).Debug()
		return err
	}

	fc.info.PID = cmd.Process.Pid
	fc.firecrackerd = cmd
	fc.fcClient = fc.newFireClient()

	if err := fc.waitVMM(timeout); err != nil {
		fc.Logger().WithField("fcInit failed:", err).Debug()
		return err
	}

	fc.state.set(apiReady)

	// Store VMM information
	return fc.store.Store(store.Hypervisor, fc.info)
}

func (fc *firecracker) fcEnd() (err error) {
	span, _ := fc.trace("fcEnd")
	defer span.Finish()

	fc.Logger().Info("Stopping firecracker VM")

	defer func() {
		if err != nil {
			fc.Logger().Info("fcEnd failed")
		} else {
			fc.Logger().Info("Firecracker VM stopped")
		}
	}()

	pid := fc.info.PID

	// Check if VM process is running, in case it is not, let's
	// return from here.
	if err = syscall.Kill(pid, syscall.Signal(0)); err != nil {
		return nil
	}

	// Send a SIGTERM to the VM process to try to stop it properly
	if err = syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}

	// Wait for the VM process to terminate
	tInit := time.Now()
	for {
		if err = syscall.Kill(pid, syscall.Signal(0)); err != nil {
			return nil
		}

		if time.Since(tInit).Seconds() >= fcStopSandboxTimeout {
			fc.Logger().Warnf("VM still running after waiting %ds", fcStopSandboxTimeout)
			break
		}

		// Let's avoid to run a too busy loop
		time.Sleep(time.Duration(50) * time.Millisecond)
	}

	// Let's try with a hammer now, a SIGKILL should get rid of the
	// VM process.
	return syscall.Kill(pid, syscall.SIGKILL)
}

func (fc *firecracker) client() *client.Firecracker {
	span, _ := fc.trace("client")
	defer span.Finish()

	if fc.fcClient == nil {
		fc.fcClient = fc.newFireClient()
	}

	return fc.fcClient
}

func (fc *firecracker) fcSetBootSource(path, params string) error {
	span, _ := fc.trace("fcSetBootSource")
	defer span.Finish()
	fc.Logger().WithFields(logrus.Fields{"kernel-path": path,
		"kernel-params": params}).Debug("fcSetBootSource")

	bootSrcParams := ops.NewPutGuestBootSourceParams()
	src := &models.BootSource{
		KernelImagePath: &path,
		BootArgs:        params,
	}
	bootSrcParams.SetBody(src)

	_, err := fc.client().Operations.PutGuestBootSource(bootSrcParams)
	return err
}

func (fc *firecracker) fcSetVMRootfs(path string) error {
	span, _ := fc.trace("fcSetVMRootfs")
	defer span.Finish()
	fc.Logger().WithField("VM-rootfs-path", path).Debug()

	driveID := "rootfs"
	driveParams := ops.NewPutGuestDriveByIDParams()
	driveParams.SetDriveID(driveID)
	isReadOnly := true
	//Add it as a regular block device
	//This allows us to use a paritioned root block device
	isRootDevice := false
	drive := &models.Drive{
		DriveID:      &driveID,
		IsReadOnly:   &isReadOnly,
		IsRootDevice: &isRootDevice,
		PathOnHost:   &path,
	}
	driveParams.SetBody(drive)
	_, err := fc.client().Operations.PutGuestDriveByID(driveParams)
	return err
}

func (fc *firecracker) fcSetVMBaseConfig(mem int64, vcpus int64, htEnabled bool) error {
	span, _ := fc.trace("fcSetVMBaseConfig")
	defer span.Finish()
	fc.Logger().WithFields(logrus.Fields{"mem": mem,
		"vcpus":     vcpus,
		"htEnabled": htEnabled}).Debug("fcSetVMBaseConfig")

	param := ops.NewPutMachineConfigurationParams()
	cfg := &models.MachineConfiguration{
		HtEnabled:  &htEnabled,
		MemSizeMib: &mem,
		VcpuCount:  &vcpus,
	}
	param.SetBody(cfg)
	_, err := fc.client().Operations.PutMachineConfiguration(param)
	return err
}

func (fc *firecracker) fcStartVM() error {
	fc.Logger().Info("start firecracker virtual machine")
	span, _ := fc.trace("fcStartVM")
	defer span.Finish()

	fc.Logger().Info("Starting VM")

	fc.fcClient = fc.newFireClient()

	actionParams := ops.NewCreateSyncActionParams()
	actionType := "InstanceStart"
	actionInfo := &models.InstanceActionInfo{
		ActionType: &actionType,
	}
	actionParams.SetInfo(actionInfo)
	_, err := fc.client().Operations.CreateSyncAction(actionParams)
	if err != nil {
		return err
	}

	fc.state.set(vmReady)

	return nil
}

// startSandbox will start the hypervisor for the given sandbox.
// In the context of firecracker, this will start the hypervisor,
// for configuration, but not yet start the actual virtual machine
func (fc *firecracker) startSandbox(timeout int) error {
	span, _ := fc.trace("startSandbox")
	defer span.Finish()

	err := fc.fcInit(fcTimeout)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			fc.fcEnd()
		}
	}()

	if err := fc.fcSetVMBaseConfig(int64(fc.config.MemorySize),
		int64(fc.config.NumVCPUs),
		false); err != nil {
		return err
	}

	kernelPath, err := fc.config.KernelAssetPath()
	if err != nil {
		return err
	}

	kernelParams := append(fc.config.KernelParams, fcKernelParams...)
	strParams := SerializeParams(kernelParams, "=")
	formattedParams := strings.Join(strParams, " ")

	fc.fcSetBootSource(kernelPath, formattedParams)

	image, err := fc.config.InitrdAssetPath()
	if err != nil {
		return err
	}

	if image == "" {
		image, err = fc.config.ImageAssetPath()
		if err != nil {
			return err
		}
	}

	fc.fcSetVMRootfs(image)
	fc.createDiskPool()

	for _, d := range fc.pendingDevices {
		if err = fc.addDevice(d.dev, d.devType); err != nil {
			return err
		}
	}

	if err := fc.fcStartVM(); err != nil {
		return err
	}

	return fc.waitVMM(timeout)
}

func fcDriveIndexToID(i int) string {
	return "drive_" + strconv.Itoa(i)
}

func (fc *firecracker) createDiskPool() error {
	span, _ := fc.trace("createDiskPool")
	defer span.Finish()

	for i := 0; i < fcDiskPoolSize; i++ {
		driveID := fcDriveIndexToID(i)
		driveParams := ops.NewPutGuestDriveByIDParams()
		driveParams.SetDriveID(driveID)
		isReadOnly := false
		isRootDevice := false

		// Create a temporary file as a placeholder backend for the drive
		hostURL, err := fc.store.Raw("")
		if err != nil {
			return err
		}

		// We get a full URL from Raw(), we need to parse it.
		u, err := url.Parse(hostURL)
		if err != nil {
			return err
		}

		drive := &models.Drive{
			DriveID:      &driveID,
			IsReadOnly:   &isReadOnly,
			IsRootDevice: &isRootDevice,
			PathOnHost:   &u.Path,
		}
		driveParams.SetBody(drive)
		_, err = fc.client().Operations.PutGuestDriveByID(driveParams)
		if err != nil {
			return err
		}
	}

	return nil
}

// stopSandbox will stop the Sandbox's VM.
func (fc *firecracker) stopSandbox() (err error) {
	span, _ := fc.trace("stopSandbox")
	defer span.Finish()

	return fc.fcEnd()
}

func (fc *firecracker) pauseSandbox() error {
	return nil
}

func (fc *firecracker) saveSandbox() error {
	return nil
}

func (fc *firecracker) resumeSandbox() error {
	return nil
}

func (fc *firecracker) fcAddVsock(vs kataVSOCK) error {
	span, _ := fc.trace("fcAddVsock")
	defer span.Finish()

	vsockParams := ops.NewPutGuestVsockByIDParams()
	vsockID := "root"
	ctxID := int64(vs.contextID)
	vsock := &models.Vsock{
		GuestCid: &ctxID,
		ID:       &vsockID,
	}
	vsockParams.SetID(vsockID)
	vsockParams.SetBody(vsock)
	_, _, err := fc.client().Operations.PutGuestVsockByID(vsockParams)
	if err != nil {
		return err
	}
	//Still racy. There is no way to send an fd to the firecracker
	//REST API. We could release this just before we start the instance
	//but even that will not eliminate the race
	vs.vhostFd.Close()
	return nil
}

func (fc *firecracker) fcAddNetDevice(endpoint Endpoint) error {
	span, _ := fc.trace("fcAddNetDevice")
	defer span.Finish()

	cfg := ops.NewPutGuestNetworkInterfaceByIDParams()
	ifaceID := endpoint.Name()
	ifaceCfg := &models.NetworkInterface{
		AllowMmdsRequests: false,
		GuestMac:          endpoint.HardwareAddr(),
		IfaceID:           &ifaceID,
		HostDevName:       &endpoint.NetworkPair().TapInterface.TAPIface.Name,
	}
	cfg.SetBody(ifaceCfg)
	cfg.SetIfaceID(ifaceID)
	_, err := fc.client().Operations.PutGuestNetworkInterfaceByID(cfg)
	return err
}

func (fc *firecracker) fcAddBlockDrive(drive config.BlockDrive) error {
	span, _ := fc.trace("fcAddBlockDrive")
	defer span.Finish()

	driveID := drive.ID
	driveParams := ops.NewPutGuestDriveByIDParams()
	driveParams.SetDriveID(driveID)
	isReadOnly := false
	isRootDevice := false
	driveFc := &models.Drive{
		DriveID:      &driveID,
		IsReadOnly:   &isReadOnly,
		IsRootDevice: &isRootDevice,
		PathOnHost:   &drive.File,
	}
	driveParams.SetBody(driveFc)
	_, err := fc.client().Operations.PutGuestDriveByID(driveParams)
	return err
}

// Firecracker supports replacing the host drive used once the VM has booted up
func (fc *firecracker) fcUpdateBlockDrive(drive config.BlockDrive) error {
	span, _ := fc.trace("fcUpdateBlockDrive")
	defer span.Finish()

	// Use the global block index as an index into the pool of the devices
	// created for firecracker.
	driveID := fcDriveIndexToID(drive.Index)
	driveParams := ops.NewPatchGuestDriveByIDParams()
	driveParams.SetDriveID(driveID)

	driveFc := &models.PartialDrive{
		DriveID:    &driveID,
		PathOnHost: &drive.File, //This is the only property that can be modified
	}
	driveParams.SetBody(driveFc)
	_, err := fc.client().Operations.PatchGuestDriveByID(driveParams)
	if err != nil {
		return err
	}

	// Rescan needs to used only if the VM is running
	if fc.vmRunning() {
		actionParams := ops.NewCreateSyncActionParams()
		actionType := "BlockDeviceRescan"
		actionInfo := &models.InstanceActionInfo{
			ActionType: &actionType,
			Payload:    driveID,
		}
		actionParams.SetInfo(actionInfo)
		_, err = fc.client().Operations.CreateSyncAction(actionParams)
		if err != nil {
			return err
		}
	}

	return nil
}

// addDevice will add extra devices to firecracker.  Limited to configure before the
// virtual machine starts.  Devices include drivers and network interfaces only.
func (fc *firecracker) addDevice(devInfo interface{}, devType deviceType) error {
	span, _ := fc.trace("addDevice")
	defer span.Finish()

	fc.state.RLock()
	defer fc.state.RUnlock()

	if fc.state.state == notReady {
		dev := firecrackerDevice{
			dev:     devInfo,
			devType: devType,
		}
		fc.Logger().Info("FC not ready, queueing device")
		fc.pendingDevices = append(fc.pendingDevices, dev)
		return nil
	}

	switch v := devInfo.(type) {
	case Endpoint:
		fc.Logger().WithField("device-type-endpoint", devInfo).Info("Adding device")
		return fc.fcAddNetDevice(v)
	case config.BlockDrive:
		fc.Logger().WithField("device-type-blockdrive", devInfo).Info("Adding device")
		return fc.fcAddBlockDrive(v)
	case kataVSOCK:
		fc.Logger().WithField("device-type-vsock", devInfo).Info("Adding device")
		return fc.fcAddVsock(v)
	default:
		fc.Logger().WithField("unknown-device-type", devInfo).Error("Adding device")
	}

	return nil
}

// hotplugAddDevice supported in Firecracker VMM
func (fc *firecracker) hotplugAddDevice(devInfo interface{}, devType deviceType) (interface{}, error) {
	span, _ := fc.trace("hotplugAddDevice")
	defer span.Finish()

	switch devType {
	case blockDev:
		//The drive placeholder has to exist prior to Update
		return nil, fc.fcUpdateBlockDrive(*devInfo.(*config.BlockDrive))
	default:
		fc.Logger().WithFields(logrus.Fields{"devInfo": devInfo,
			"deviceType": devType}).Warn("hotplugAddDevice: unsupported device")
		return nil, fmt.Errorf("hotplugAddDevice: unsupported device: devInfo:%v, deviceType%v",
			devInfo, devType)
	}
}

// hotplugRemoveDevice supported in Firecracker VMM, but no-op
func (fc *firecracker) hotplugRemoveDevice(devInfo interface{}, devType deviceType) (interface{}, error) {
	return nil, nil
}

// getSandboxConsole builds the path of the console where we can read
// logs coming from the sandbox.
//
// we can get logs from firecracker itself; WIP on enabling.  Who needs
// logs when you're just hacking?
func (fc *firecracker) getSandboxConsole(id string) (string, error) {
	return "", nil
}

func (fc *firecracker) disconnect() {
	fc.state.set(notReady)
}

// Adds all capabilities supported by firecracker implementation of hypervisor interface
func (fc *firecracker) capabilities() types.Capabilities {
	span, _ := fc.trace("capabilities")
	defer span.Finish()
	var caps types.Capabilities
	caps.SetFsSharingUnsupported()
	caps.SetBlockDeviceHotplugSupport()

	return caps
}

func (fc *firecracker) hypervisorConfig() HypervisorConfig {
	return fc.config
}

func (fc *firecracker) resizeMemory(reqMemMB uint32, memoryBlockSizeMB uint32, probe bool) (uint32, memoryDevice, error) {
	return 0, memoryDevice{}, nil
}

func (fc *firecracker) resizeVCPUs(reqVCPUs uint32) (currentVCPUs uint32, newVCPUs uint32, err error) {
	return 0, 0, nil
}

// This is used to apply cgroup information on the host.
//
// As suggested by https://github.com/firecracker-microvm/firecracker/issues/718,
// let's use `ps -T -p <pid>` to get fc vcpu info.
func (fc *firecracker) getThreadIDs() (vcpuThreadIDs, error) {
	var vcpuInfo vcpuThreadIDs

	vcpuInfo.vcpus = make(map[int]int)
	parent, err := utils.NewProc(fc.info.PID)
	if err != nil {
		return vcpuInfo, err
	}
	children, err := parent.Children()
	if err != nil {
		return vcpuInfo, err
	}
	for _, child := range children {
		comm, err := child.Comm()
		if err != nil {
			return vcpuInfo, errors.New("Invalid fc thread info")
		}
		if !strings.HasPrefix(comm, "fc_vcpu") {
			continue
		}
		cpus := strings.SplitAfter(comm, "fc_vcpu")
		if len(cpus) != 2 {
			return vcpuInfo, errors.Errorf("Invalid fc thread info: %v", comm)
		}
		cpuID, err := strconv.ParseInt(cpus[1], 10, 32)
		if err != nil {
			return vcpuInfo, errors.Wrapf(err, "Invalid fc thread info: %v", comm)
		}
		vcpuInfo.vcpus[int(cpuID)] = child.PID
	}

	return vcpuInfo, nil
}

func (fc *firecracker) cleanup() error {
	return nil
}

func (fc *firecracker) pid() int {
	return fc.info.PID
}

func (fc *firecracker) fromGrpc(ctx context.Context, hypervisorConfig *HypervisorConfig, store *store.VCStore, j []byte) error {
	return errors.New("firecracker is not supported by VM cache")
}

func (fc *firecracker) toGrpc() ([]byte, error) {
	return nil, errors.New("firecracker is not supported by VM cache")
}
