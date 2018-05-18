package qemubuild

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"

	"github.com/pkg/errors"
	schematypes "github.com/taskcluster/go-schematypes"
	"github.com/taskcluster/taskcluster-worker/commands/qemu-run"
	"github.com/taskcluster/taskcluster-worker/engines/qemu/image"
	"github.com/taskcluster/taskcluster-worker/engines/qemu/network"
	"github.com/taskcluster/taskcluster-worker/engines/qemu/vm"
	"github.com/taskcluster/taskcluster-worker/runtime"
	"github.com/taskcluster/taskcluster-worker/runtime/ioext"
	"github.com/taskcluster/taskcluster-worker/runtime/util"
)

var debug = util.Debug("qemubuild")

func buildImage(
	monitor runtime.Monitor,
	inputFile, outputFile string,
	fromImage bool,
	vncPort int,
	boot, cdrom string,
	linuxBootOptions vm.LinuxBootOptions,
	size int,
) error {
	// Find absolute outputFile
	outputFile, err := filepath.Abs(outputFile)
	if err != nil {
		monitor.Error("Failed to resolve output file, error: ", err)
		return err
	}

	// Create temp folder for the image
	tempFolder, err := ioutil.TempDir("", "taskcluster-worker-build-image-")
	if err != nil {
		monitor.Error("Failed to create temporary folder, error: ", err)
		return err
	}
	defer os.RemoveAll(tempFolder)

	var img *image.MutableImage
	if !fromImage {
		// Read machine definition
		machine, err2 := newMachineFromFile(inputFile)
		if err2 != nil {
			monitor.Error("Failed to load machine file from ", inputFile, " error: ", err2)
			return err2
		}

		// Construct MutableImage
		monitor.Info("Creating MutableImage")
		img, err2 = image.NewMutableImage(tempFolder, size, machine)
		if err2 != nil {
			monitor.Error("Failed to create image, error: ", err2)
			return err2
		}
	} else {
		img, err = image.NewMutableImageFromFile(inputFile, tempFolder)
		if err != nil {
			monitor.Error("Failed to load image, error: ", err)
			return err
		}
	}

	// Create temp folder for sockets
	socketFolder, err := ioutil.TempDir("", "taskcluster-worker-sockets-")
	if err != nil {
		monitor.Error("Failed to create temporary folder, error: ", err)
		return err
	}
	defer os.RemoveAll(socketFolder)

	// Setup a user-space network
	monitor.Info("Creating user-space network")
	net, err := network.NewUserNetwork(tempFolder)
	if err != nil {
		monitor.Error("Failed to create user-space network, error: ", err)
		return err
	}

	// Setup logService so that logs can be posted to meta-service at:
	// http://169.254.169.254/engine/v1/log
	net.SetHandler(&logService{Destination: os.Stdout})

	// Create virtual machine
	monitor.Info("Creating virtual machine")
	machine, err := vm.NewVirtualMachine(
		img.Machine().DeriveLimits(), img, net, socketFolder,
		boot, cdrom, linuxBootOptions,
		monitor.WithTag("component", "vm"),
	)
	if err != nil {
		monitor.Error("Failed to recreated virtual-machine, error: ", err)
		return err
	}

	// Start the virtual machine
	monitor.Info("Starting virtual machine")
	machine.Start()

	// Expose VNC socket
	if vncPort != 0 {
		go qemurun.ExposeVNC(machine.VNCSocket(), vncPort, machine.Done)
	}

	// Wait for interrupt to gracefully kill everything
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)

	// Wait for virtual machine to be done, or we get interrupted
	select {
	case <-interrupted:
		machine.Kill()
		err = errors.New("SIGINT received, aborting virtual machine")
	case <-machine.Done:
		err = machine.Error
	}
	<-machine.Done
	signal.Stop(interrupted)
	defer img.Dispose()

	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			monitor.Error("QEMU error: ", string(e.Stderr))
		}
		monitor.Info("Error running virtual machine: ", err)
		return err
	}

	// Package up the finished image
	monitor.Info("Package virtual machine image")
	err = img.Package(outputFile)
	if err != nil {
		monitor.Error("Failed to package finished image, error: ", err)
		return err
	}

	return nil
}

// load vm.Machine from file without migration
func newMachineFromFile(machineFile string) (*vm.Machine, error) {
	// Read machine.json
	machineData, err := ioext.BoundedReadFile(machineFile, 1024*1024)
	if err == ioext.ErrFileTooBig {
		return nil, runtime.NewMalformedPayloadError(
			"The file 'machine.json' larger than 1MiB. JSON files must be small.")
	}
	if err != nil {
		return nil, errors.Wrap(err, "faild to read 'machine.json'")
	}

	// Parse JSON
	var data interface{}
	if err = json.Unmarshal(machineData, &data); err != nil {
		return nil, runtime.NewMalformedPayloadError(
			"Invalid JSON in 'machine.json', error: ", err)
	}

	// Validate against schema
	verr := vm.MachineSchema.Validate(data)
	if e, ok := verr.(*schematypes.ValidationError); ok {
		issues := e.Issues("machine")
		errs := make([]*runtime.MalformedPayloadError, len(issues))
		for i, issue := range issues {
			errs[i] = runtime.NewMalformedPayloadError(issue.String())
		}
		return nil, runtime.MergeMalformedPayload(append([]*runtime.MalformedPayloadError{
			runtime.NewMalformedPayloadError("Invalid machine definition in 'machine.json'"),
		}, errs...)...)
	} else if verr != nil {
		return nil, runtime.NewMalformedPayloadError("task.payload schema violation: ", verr)
	}

	// Create machine
	m := vm.NewMachine(data)

	return &m, nil
}
