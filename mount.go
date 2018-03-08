//
// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	pb "github.com/kata-containers/agent/protocols/grpc"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

const (
	type9pFs       = "9p"
	devPrefix      = "/dev/"
	timeoutHotplug = 3
	mountPerm      = os.FileMode(0755)
)

var flagList = map[string]int{
	"acl":         unix.MS_POSIXACL,
	"bind":        unix.MS_BIND,
	"defaults":    0,
	"dirsync":     unix.MS_DIRSYNC,
	"iversion":    unix.MS_I_VERSION,
	"lazytime":    unix.MS_LAZYTIME,
	"mand":        unix.MS_MANDLOCK,
	"noatime":     unix.MS_NOATIME,
	"nodev":       unix.MS_NODEV,
	"nodiratime":  unix.MS_NODIRATIME,
	"noexec":      unix.MS_NOEXEC,
	"nosuid":      unix.MS_NOSUID,
	"rbind":       unix.MS_BIND | unix.MS_REC,
	"relatime":    unix.MS_RELATIME,
	"remount":     unix.MS_REMOUNT,
	"ro":          unix.MS_RDONLY,
	"silent":      unix.MS_SILENT,
	"strictatime": unix.MS_STRICTATIME,
	"sync":        unix.MS_SYNCHRONOUS,
	"private":     unix.MS_PRIVATE,
	"shared":      unix.MS_SHARED,
	"slave":       unix.MS_SLAVE,
	"unbindable":  unix.MS_UNBINDABLE,
	"rprivate":    unix.MS_PRIVATE | unix.MS_REC,
	"rshared":     unix.MS_SHARED | unix.MS_REC,
	"rslave":      unix.MS_SLAVE | unix.MS_REC,
	"runbindable": unix.MS_UNBINDABLE | unix.MS_REC,
}

func createDestinationDir(dest string) error {
	targetPath, _ := filepath.Split(dest)

	return os.MkdirAll(targetPath, mountPerm)
}

// mount mounts a source in to a destination. This will do some bookkeeping:
// * evaluate all symlinks
// * ensure the source exists
func mount(source, destination, fsType string, flags int, options string) error {
	var absSource string

	if fsType != type9pFs {
		var err error

		absSource, err = filepath.EvalSymlinks(source)
		if err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not resolve symlink for source %v", source)
		}

		if err := ensureDestinationExists(absSource, destination, fsType); err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not create destination mount point: %v: %v",
				destination, err)
		}
	} else {
		if err := createDestinationDir(destination); err != nil {
			return err
		}
		absSource = source
	}

	if err := syscall.Mount(absSource, destination,
		fsType, uintptr(flags), options); err != nil {
		return grpcStatus.Errorf(codes.Internal, "Could not bind mount %v to %v: %v",
			absSource, destination, err)
	}

	return nil
}

// ensureDestinationExists will recursively create a given mountpoint. If directories
// are created, their permissions are initialized to mountPerm
func ensureDestinationExists(source, destination string, fsType string) error {
	fileInfo, err := os.Stat(source)
	if err != nil {
		return grpcStatus.Errorf(codes.Internal, "could not stat source location: %v",
			source)
	}

	if err := createDestinationDir(destination); err != nil {
		return grpcStatus.Errorf(codes.Internal, "could not create parent directory: %v",
			destination)
	}

	if fsType != "bind" || fileInfo.IsDir() {
		if err := os.Mkdir(destination, mountPerm); !os.IsExist(err) {
			return err
		}
	} else {
		file, err := os.OpenFile(destination, os.O_CREATE, mountPerm)
		if err != nil {
			return err
		}

		file.Close()
	}
	return nil
}

func parseMountFlagsAndOptions(optionList []string) (int, string, error) {
	var (
		flags   int
		options []string
	)

	for _, opt := range optionList {
		flag, ok := flagList[opt]
		if ok {
			flags |= flag
			continue
		}

		options = append(options, opt)
	}

	return flags, strings.Join(options, ","), nil
}

func removeMounts(mounts []string) error {
	for _, mount := range mounts {
		if err := syscall.Unmount(mount, 0); err != nil {
			return err
		}
	}

	return nil
}

type storageDriversHandler func(storage pb.Storage, spec *pb.Spec) (string, error)

var storageDriversHandlerList = map[string]storageDriversHandler{
	driver9pType:  storage9pDriverHandler,
	driverBlkType: storageBlockDeviceDriverHandler,
}

func storage9pDriverHandler(storage pb.Storage, spec *pb.Spec) (string, error) {
	return commonStorageHandler(storage, spec)
}

func storageBlockDeviceDriverHandler(storage pb.Storage, spec *pb.Spec) (string, error) {
	// First need to make sure the expected device shows up properly.
	if err := waitForDevice(storage.Source); err != nil {
		return "", err
	}

	return commonStorageHandler(storage, spec)
}

func commonStorageHandler(storage pb.Storage, spec *pb.Spec) (string, error) {
	if storage.Rootfs {
		// Mount the storage device.
		if err := mountStorage(storage); err != nil {
			return "", err
		}

		return storage.MountPoint, nil
	}

	// Update list of Mounts from OCI specification.
	updateOCIMounts(storage, spec)

	return "", nil
}

func mountStorage(storage pb.Storage) error {
	flags, options, err := parseMountFlagsAndOptions(storage.Options)
	if err != nil {
		return err
	}

	return mount(storage.Source, storage.MountPoint, storage.Fstype, flags, options)
}

func updateOCIMounts(storage pb.Storage, spec *pb.Spec) {
	if spec == nil {
		return
	}

	// Update the spec if there is a corresponding Mount.
	for idx, mnt := range spec.Mounts {
		if mnt.Destination == storage.MountPoint {
			agentLog.WithFields(logrus.Fields{
				"old-mount-source":  spec.Mounts[idx].Source,
				"new-mount-source":  storage.Source,
				"old-mount-fstype":  spec.Mounts[idx].Type,
				"new-mount-fstype":  storage.Fstype,
				"old-mount-options": spec.Mounts[idx].Options,
				"new-mount-options": storage.Options,
				"destination":       storage.MountPoint,
			}).Info("updating OCI mount entry")
			spec.Mounts[idx].Source = storage.Source
			spec.Mounts[idx].Type = storage.Fstype
			spec.Mounts[idx].Options = storage.Options
			break
		}
	}
}

func addStorages(storages []*pb.Storage, spec *pb.Spec) ([]string, error) {
	var mountList []string

	for _, storage := range storages {
		if storage == nil {
			continue
		}

		devHandler, ok := storageDriversHandlerList[storage.Driver]
		if !ok {
			return nil, grpcStatus.Errorf(codes.InvalidArgument,
				"Unknown storage driver %q", storage.Driver)
		}

		mountPoint, err := devHandler(*storage, spec)
		if err != nil {
			return nil, err
		}

		if mountPoint != "" {
			// Prepend mount point to mount list.
			mountList = append([]string{mountPoint}, mountList...)
		}
	}

	return mountList, nil
}
