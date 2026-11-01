// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
)

type enclosureFileSystem interface {
	Lstat(string) (os.FileInfo, error)
	ReadFile(string) ([]byte, error)
}

type osEnclosureFileSystem struct{}

func (osEnclosureFileSystem) Lstat(path string) (os.FileInfo, error) { return os.Lstat(path) }
func (osEnclosureFileSystem) ReadFile(path string) ([]byte, error)   { return os.ReadFile(path) }

type enclosureDevicePin struct {
	mode     os.FileMode
	statMode uint32
	rdev     uint64
}

func pinEnclosureDevice(info os.FileInfo) (enclosureDevicePin, error) {
	if info == nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice == 0 || info.Mode()&os.ModeSymlink != 0 {
		return enclosureDevicePin{}, errors.New("SES enclosure path is not a direct character device")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return enclosureDevicePin{}, errors.New("SES enclosure path exposes no Linux character-device identity")
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		return enclosureDevicePin{}, errors.New("SES enclosure path has inconsistent character-device metadata")
	}
	return enclosureDevicePin{mode: info.Mode(), statMode: stat.Mode, rdev: uint64(stat.Rdev)}, nil
}

// probeEnclosureIdentity proves both that path is currently a Linux SES
// character device (SCSI peripheral type 13) and that udev exposes a durable
// hardware identifier. The returned value is persisted at physical verify
// time and compared again immediately before locate dispatch.
func probeEnclosureIdentity(ctx context.Context, fsys enclosureFileSystem, runner execx.Runner, path string) (string, error) {
	if ctx == nil {
		return "", errors.New("nil SES probe context")
	}
	if fsys == nil || runner == nil {
		return "", errors.New("SES probe requires filesystem and command authority")
	}
	if !inventory.ValidSESDevicePath(path) {
		return "", errors.New("SES enclosure must use a canonical /dev/sgN or /dev/bsg/H:C:T:L path")
	}
	info, err := fsys.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect SES enclosure device: %w", err)
	}
	initialPin, err := pinEnclosureDevice(info)
	if err != nil {
		return "", err
	}
	typePath, err := enclosureTypePath(path)
	if err != nil {
		return "", err
	}
	peripheralType, err := fsys.ReadFile(typePath)
	if err != nil {
		return "", fmt.Errorf("read SES peripheral type: %w", err)
	}
	if strings.TrimSpace(string(peripheralType)) != "13" {
		return "", fmt.Errorf("device %q is SCSI peripheral type %q, not enclosure-services type 13", path, strings.TrimSpace(string(peripheralType)))
	}
	result, err := runner.Run(ctx, execx.Command{
		Program: "udevadm", Args: []string{"info", "--query=property", "--name", path},
		Capability: execx.CapabilityObserve, Risk: domain.RiskRead,
	})
	if err != nil {
		return "", fmt.Errorf("collect SES udev identity: %w", err)
	}
	finalInfo, err := fsys.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("re-inspect SES enclosure device: %w", err)
	}
	finalPin, err := pinEnclosureDevice(finalInfo)
	if err != nil {
		return "", fmt.Errorf("re-inspect SES enclosure device: %w", err)
	}
	if finalPin != initialPin {
		return "", errors.New("SES enclosure device type, mode, or rdev changed during probe")
	}
	identity, err := enclosureIdentityFromUdev(result.Stdout)
	if err != nil {
		return "", err
	}
	return identity, nil
}

func enclosureTypePath(path string) (string, error) {
	switch {
	case strings.HasPrefix(path, "/dev/sg"):
		return filepath.Join("/sys/class/scsi_generic", filepath.Base(path), "device/type"), nil
	case strings.HasPrefix(path, "/dev/bsg/"):
		return filepath.Join("/sys/class/bsg", filepath.Base(path), "device/type"), nil
	default:
		return "", errors.New("unsupported SES device path")
	}
}

func enclosureIdentityFromUdev(data []byte) (string, error) {
	properties := make(map[string]string)
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		key, value, present := strings.Cut(line, "=")
		if !present || key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if previous, duplicate := properties[key]; duplicate && previous != value {
			return "", fmt.Errorf("udev returned conflicting %s values", key)
		}
		properties[key] = value
	}
	for _, key := range []string{"ID_WWN_WITH_EXTENSION", "ID_WWN"} {
		if identity, ok := inventory.CanonicalSESWWN(properties[key]); ok {
			return identity, nil
		}
	}
	for _, key := range []string{"ID_SCSI_SERIAL", "ID_SERIAL_SHORT"} {
		if identity, ok := inventory.CanonicalSESSerial(properties[key]); ok {
			return identity, nil
		}
	}
	return "", errors.New("SES enclosure exposes no reliable WWN or hardware serial through udev")
}
