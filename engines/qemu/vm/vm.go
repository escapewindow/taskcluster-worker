package vm

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/fsnotify/fsnotify"
	"github.com/taskcluster/slugid-go/slugid"
)

const (
	vncSocketFile = "vnc.sock"
	qmpSocketFile = "qmp.sock"
)

// VirtualMachine holds the QEMU process and associated resources.
// This is useful as the VM remains alive in the ResultSet stage, as we use
// guest tools to copy files from the virtual machine.
type VirtualMachine struct {
	m            sync.Mutex // Protect access to resources
	started      bool
	network      Network
	image        Image
	socketFolder string
	qemu         *exec.Cmd
	qemuDone     chan<- struct{}
	Done         <-chan struct{} // Closed when the virtual machine is done
	Error        error           // Error, to be read after Done is closed
	log          *logrus.Entry
}

// NewVirtualMachine constructs a new virtual machine.
func NewVirtualMachine(
	image Image, network Network, socketFolder, cdrom1, cdrom2 string,
	log *logrus.Entry,
) *VirtualMachine {
	// Create a sub-folder in the socketFolder
	socketFolder = filepath.Join(socketFolder, slugid.Nice())

	// Construct virtual machine
	vm := &VirtualMachine{
		socketFolder: socketFolder,
		network:      network,
		image:        image,
		log:          log,
	}

	vncSocket := filepath.Join(vm.socketFolder, vncSocketFile)
	qmpSocket := filepath.Join(vm.socketFolder, qmpSocketFile)

	// Construct options for QEMU
	type opts map[string]string
	arg := func(kind string, opts opts) string {
		result := kind
		for k, v := range opts {
			if result != "" {
				result += ","
			}
			result += k + "=" + v
		}
		return result
	}
	options := []string{
		"-name", "qemu-guest",
		// TODO: Add -enable-kvm (configurable so can be disabled in tests)
		"-machine", arg("pc-i440fx-2.1", opts{
			"accel": "kvm",
			// TODO: Configure additional options")
		}),
		"-m", "512", // TODO: Make memory configurable
		"-realtime", "mlock=off", // TODO: Enable for things like talos
		// TODO: fit to system HT, see: https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-devices-system-cpu
		// TODO: Configure CPU instruction sets: http://forum.ipfire.org/viewtopic.php?t=12642
		"-smp", "cpus=2,sockets=2,cores=1,threads=1",
		"-uuid", vm.image.Machine().UUID,
		"-no-user-config", "-nodefaults",
		"-rtc", "base=utc", // TODO: Allow clock=vm for loadvm with windows
		"-boot", "menu=off,strict=on",
		"-k", vm.image.Machine().Keyboard.Layout,
		"-device", arg("vmware-svga", opts{
			// TODO: Investigate if we can use vmware
			// VGA ought to be the safe choice here
			"id":        "video-0",
			"vgamem_mb": "64", // TODO: Customize VGA memory
			"bus":       "pci.0",
			"addr":      "0x2", // QEMU uses PCI 0x2 for VGA by default
		}),
		"-device", arg("nec-usb-xhci", opts{
			"id":   "usb",
			"bus":  "pci.0",
			"addr": "0x3", // Always put USB on PCI 0x3
		}),
		"-device", arg("virtio-balloon-pci", opts{
			"id":   "balloon-0",
			"bus":  "pci.0",
			"addr": "0x4", // Always put balloon on PCI 0x4
		}),
		"-netdev", vm.network.NetDev("netdev-0"),
		"-device", arg(vm.image.Machine().Network.Device, opts{
			"netdev": "netdev-0",
			"id":     "nic0",
			"mac":    vm.image.Machine().Network.MAC,
			"bus":    "pci.0",
			"addr":   "0x5", // Always put network on PCI 0x5
		}),
		// Reserve PCI 0x6 for sound device/controller
		"-device", arg("usb-kbd", opts{
			"id":   "keyboard-0",
			"bus":  "usb.0",
			"port": "1", // USB port offset starts at 1
		}),
		"-device", arg("usb-mouse", opts{
			"id":   "mouse-0",
			"bus":  "usb.0",
			"port": "2",
		}),
		"-vnc", arg("unix:"+vncSocket, opts{
			"share": "force-shared",
		}),
		"-chardev", "socket,id=qmpsocket,path=" + qmpSocket + ",nowait,server=on",
		"-mon", "chardev=qmpsocket,mode=control",
		"-drive", arg("", opts{
			"file":   vm.image.DiskFile(),
			"if":     "none",
			"id":     "boot-disk",
			"cache":  "unsafe", // TODO: Reconsider 'native' w. cache not 'unsafe'
			"aio":    "threads",
			"format": vm.image.Format(),
			"werror": "report",
			"rerror": "report",
		}),
		"-device", arg("virtio-blk-pci", opts{
			"scsi":      "off",
			"bus":       "pci.0",
			"addr":      "0x8", // Start disks as 0x8, reserve 0x7 for future
			"drive":     "boot-disk",
			"id":        "virtio-disk0",
			"bootindex": "1",
		}),
		// TODO: Add cache volumes
	}

	// Add optional sound device
	if vm.image.Machine().Sound != nil {
		if vm.image.Machine().Sound.Controller == "pci" {
			options = append(options, []string{
				"-device", arg(vm.image.Machine().Sound.Device, opts{
					"id":   "sound-0",
					"bus":  "pci.0",
					"addr": "0x6", // Always put sound on PCI 0x6
				}),
			}...)
		} else {
			options = append(options, []string{
				"-device", arg(vm.image.Machine().Sound.Controller, opts{
					"id":   "sound-0",
					"bus":  "pci.0",
					"addr": "0x6", // Always put sound on PCI 0x6
				}),
				"-device", arg(vm.image.Machine().Sound.Device, opts{
					"id":  "sound-0-codec-0",
					"bus": "sound-0.0",
					"cad": "0",
				}),
			}...)
		}
	}

	if cdrom1 != "" {
		options = append(options, []string{
			"-drive", arg("", opts{
				"file":   cdrom1,
				"if":     "none",
				"id":     "cdrom1",
				"cache":  "unsafe",
				"aio":    "threads", // TODO: Reconsider 'native' w. cache not 'unsafe'
				"format": "raw",
				"werror": "report",
				"rerror": "report",
			}) + ",readonly",
			"-device", arg("ide-cd", opts{
				"bootindex": "2",
				"drive":     "cdrom1",
				"id":        "ide-cd1",
				"bus":       "ide.0",
				"unit":      "0",
			}),
		}...)
	}
	if cdrom2 != "" {
		options = append(options, []string{
			"-drive", arg("", opts{
				"file":   cdrom2,
				"if":     "none",
				"id":     "cdrom2",
				"cache":  "unsafe",
				"aio":    "threads", // TODO: Reconsider 'native' w. cache not 'unsafe'
				"format": "raw",
				"werror": "report",
				"rerror": "report",
			}) + ",readonly",
			"-device", arg("ide-cd", opts{
				"bootindex": "3",
				"drive":     "cdrom2",
				"id":        "ide-cd2",
				"bus":       "ide.0",
				"unit":      "1",
			}),
		}...)
	}

	// Create done channel
	qemuDone := make(chan struct{})
	vm.qemuDone = qemuDone
	vm.Done = qemuDone

	// Create QEMU process
	vm.qemu = exec.Command("qemu-system-x86_64", options...)

	return vm
}

// SetHTTPHandler sets the HTTP handler for the meta-data service.
func (vm *VirtualMachine) SetHTTPHandler(handler http.Handler) {
	vm.m.Lock()
	defer vm.m.Unlock()
	if vm.network != nil {
		// Ignore the case where network has been released
		vm.network.SetHandler(handler)
	}
}

// Start the virtual machine.
func (vm *VirtualMachine) Start() {
	vm.m.Lock()
	if vm.started {
		vm.m.Unlock()
		panic("virtual machine instance have already been started once")
	}
	vm.started = true
	vm.m.Unlock()

	stdout, stdoutWriter := io.Pipe()
	stderr, stderrWriter := io.Pipe()
	vm.qemu.Stdout = stdoutWriter
	vm.qemu.Stderr = stderrWriter

	// Start monitor socketFolder for vnc and qmp sockets
	socketsReady, err := vm.waitForSockets()
	if err != nil {
		vm.log.Errorf("Error configuring socketFolder monitoring, error: %s", err)
		vm.Error = err
		close(vm.qemuDone)
		return
	}

	// Start QEMU
	vm.Error = vm.qemu.Start()
	if vm.Error != nil {
		close(vm.qemuDone)
		return
	}

	// Forward stdout to log
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			vm.log.Info("QEMU: ", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			vm.log.Error("Error reading QEMU stdout, error: ", err)
		}
	}()

	// Forward stderr to log
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			vm.log.Error("QEMU: ", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			vm.log.Error("Error reading QEMU stderr, error: ", err)
		}
	}()

	// Start QEMU and wait for it to finish before closing Done
	go func(vm *VirtualMachine) {
		// Wait for QEMU to be done
		werr := vm.qemu.Wait()

		// Release network and image
		vm.m.Lock()
		defer vm.m.Unlock()

		// Set error, if any and not already set
		if vm.Error == nil {
			vm.Error = werr
		}

		// Close output pipes
		stdoutWriter.Close()
		stderrWriter.Close()

		vm.network.Release()
		vm.network = nil
		vm.image.Release()
		vm.image = nil

		// Remove socket folder
		os.RemoveAll(vm.socketFolder)
		vm.socketFolder = ""

		// Notify everybody that the VM is stopped
		// Ensure resources are freed first, otherwise we'll race with resources
		// against the next task. If the number of resources is limiting the
		// number of concurrent tasks we can run.
		// This is usually the case, so race would happen at full capacity.
		close(vm.qemuDone)
	}(vm)

	// Wait for vncSocket and qmpSocket to appear, or qemu to crash
	select {
	case err := <-socketsReady:
		if err != nil {
			vm.log.Errorf("Error monitoring socketFolder, error: %s", err)
			vm.m.Lock()
			if vm.Error != nil {
				vm.Error = err
			}
			vm.m.Unlock()
			vm.Kill()
		}
	case <-vm.Done:
	}
}

// Kill the virtual machine, can only be called after Start()
func (vm *VirtualMachine) Kill() {
	select {
	case <-vm.Done:
		return // We're obviously not running, so we must be done
	default:
		vm.qemu.Process.Kill()
	}
}

// VNCSocket returns the path to VNC socket, empty-string if closed.
func (vm *VirtualMachine) VNCSocket() string {
	// Lock access to vncSocket
	vm.m.Lock()
	defer vm.m.Unlock()

	if vm.socketFolder == "" {
		return ""
	}

	return filepath.Join(vm.socketFolder, vncSocketFile)
}

// waitForSockets will monitor socketFolder and return a channel that is closed
// when vncSocketFile and qmpSocketFile have been created.
func (vm *VirtualMachine) waitForSockets() (<-chan error, error) {
	done := make(chan error)

	// Setup file monitoring, if there is an error here we panic, this should
	// always be reliable.
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("Failed to setup file system monitoring, error: %s", err)
	}
	err = w.Add(vm.socketFolder)
	if err != nil {
		return nil, fmt.Errorf("Failed to monitor socket folder, error: %s", err)
	}

	// Handle events, and close the done channel when sockets are ready
	go func() {
		vncReady := false
		qmpReady := false
		for !vncReady || !qmpReady {
			select {
			case e := <-w.Events:
				if e.Op == fsnotify.Create {
					if e.Name == vncSocketFile {
						vncReady = true
					}
					if e.Name == qmpSocketFile {
						qmpReady = true
					}
				}
			case <-vm.Done:
				// Stop monitoring if QEMU has crashed
				w.Close()
				return
			case <-time.After(90 * time.Second):
				done <- fmt.Errorf("vnc and qmp sockets didn't show up in 90s")
				w.Close()
				return
			case err := <-w.Errors:
				done <- fmt.Errorf("Error monitoring file system, error: %s", err)
				w.Close()
				return
			}
		}
		w.Close()
		close(done)
	}()

	return done, nil
}
