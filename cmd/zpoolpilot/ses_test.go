// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

type fakeEnclosureFS struct {
	mode         fs.FileMode
	rdev         uint64
	files        map[string][]byte
	lstatResults []fakeEnclosureInfo
	lstatCalls   *int
}

func (fake fakeEnclosureFS) Lstat(string) (os.FileInfo, error) {
	if fake.lstatCalls != nil {
		call := *fake.lstatCalls
		(*fake.lstatCalls)++
		if call < len(fake.lstatResults) {
			return fake.lstatResults[call], nil
		}
	}
	return fakeEnclosureInfo{mode: fake.mode, rdev: fake.rdev}, nil
}

func (fake fakeEnclosureFS) ReadFile(path string) ([]byte, error) {
	data, ok := fake.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

type fakeEnclosureInfo struct {
	mode fs.FileMode
	rdev uint64
}

func (info fakeEnclosureInfo) Name() string       { return "sg2" }
func (info fakeEnclosureInfo) Size() int64        { return 0 }
func (info fakeEnclosureInfo) Mode() fs.FileMode  { return info.mode }
func (info fakeEnclosureInfo) ModTime() time.Time { return time.Time{} }
func (info fakeEnclosureInfo) IsDir() bool        { return false }
func (info fakeEnclosureInfo) Sys() any {
	mode := uint32(info.mode.Perm())
	if info.mode&os.ModeCharDevice != 0 {
		mode |= syscall.S_IFCHR
	} else if info.mode&os.ModeDevice != 0 {
		mode |= syscall.S_IFBLK
	} else {
		mode |= syscall.S_IFREG
	}
	return &syscall.Stat_t{Mode: mode, Rdev: info.rdev}
}

func TestProbeEnclosureIdentityRequiresSESDeviceAndStableIdentity(t *testing.T) {
	filesystem := fakeEnclosureFS{
		mode: os.ModeDevice | os.ModeCharDevice,
		rdev: 0x1234,
		files: map[string][]byte{
			"/sys/class/scsi_generic/sg2/device/type": []byte("13\n"),
		},
	}
	runner := execx.NewFakeRunner(true, execx.FakeResponse{Stdout: []byte("ID_WWN=0x5000C500DEADBEEF\n")})
	identity, err := probeEnclosureIdentity(context.Background(), filesystem, runner, "/dev/sg2")
	if err != nil {
		t.Fatal(err)
	}
	if identity != "wwn:5000c500deadbeef" {
		t.Fatalf("identity = %q", identity)
	}

	filesystem.files["/sys/class/scsi_generic/sg2/device/type"] = []byte("0\n")
	if _, err := probeEnclosureIdentity(context.Background(), filesystem, execx.NewFakeRunner(true), "/dev/sg2"); err == nil || !strings.Contains(err.Error(), "type 13") {
		t.Fatalf("non-SES device accepted: %v", err)
	}
	filesystem.mode = 0
	if _, err := probeEnclosureIdentity(context.Background(), filesystem, execx.NewFakeRunner(true), "/dev/sg2"); err == nil || !strings.Contains(err.Error(), "character device") {
		t.Fatalf("regular file accepted: %v", err)
	}
}

func TestEnclosureIdentityRejectsPlaceholderValues(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"zero WWN", "ID_WWN=0x0000000000000000\n"},
		{"all-f WWN", "ID_WWN=0xffffffffffffffff\n"},
		{"generic serial", "ID_SCSI_SERIAL=generic\n"},
		{"same-character serial", "ID_SCSI_SERIAL=000000\n"},
		{"sequential serial", "ID_SCSI_SERIAL=123456\n"},
		{"punctuated zeros", "ID_SCSI_SERIAL=0000-0000\n"},
		{"unknown suffix", "ID_SCSI_SERIAL=UNKNOWN123\n"},
		{"OEM placeholder", "ID_SCSI_SERIAL=To_be_filled_by_O.E.M.\n"},
		{"blank serial", "ID_SCSI_SERIAL=\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := enclosureIdentityFromUdev([]byte(test.data)); err == nil {
				t.Fatalf("placeholder identity accepted from %q", test.data)
			}
		})
	}
}

func TestEnclosureIdentityPrefersSCSISerialAndRejectsConflicts(t *testing.T) {
	identity, err := enclosureIdentityFromUdev([]byte("ID_SCSI_SERIAL=controller-serial\nID_SERIAL_SHORT=weaker-short\n"))
	if err != nil {
		t.Fatal(err)
	}
	if identity != "scsi-serial:controller-serial" {
		t.Fatalf("identity = %q, want ID_SCSI_SERIAL", identity)
	}
	_, err = enclosureIdentityFromUdev([]byte("ID_WWN=5000c500deadbeef\nID_WWN=5000c500cafefeed\n"))
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting udev identity accepted: %v", err)
	}
}

func TestProbeEnclosureIdentityPropagatesUdevFailure(t *testing.T) {
	filesystem := fakeEnclosureFS{
		mode: os.ModeDevice | os.ModeCharDevice,
		rdev: 0x1234,
		files: map[string][]byte{
			"/sys/class/bsg/1:2:3:4/device/type": []byte("13\n"),
		},
	}
	want := errors.New("udev unavailable")
	runner := execx.NewFakeRunner(true, execx.FakeResponse{Err: want})
	if _, err := probeEnclosureIdentity(context.Background(), filesystem, runner, "/dev/bsg/1:2:3:4"); !errors.Is(err, want) {
		t.Fatalf("probe error = %v, want wrapped udev failure", err)
	}
}

func TestProbeEnclosureIdentityRejectsRdevChangeDuringProbe(t *testing.T) {
	calls := 0
	filesystem := fakeEnclosureFS{
		files: map[string][]byte{
			"/sys/class/scsi_generic/sg2/device/type": []byte("13\n"),
		},
		lstatResults: []fakeEnclosureInfo{
			{mode: os.ModeDevice | os.ModeCharDevice, rdev: 0x1234},
			{mode: os.ModeDevice | os.ModeCharDevice, rdev: 0x5678},
		},
		lstatCalls: &calls,
	}
	runner := execx.NewFakeRunner(true, execx.FakeResponse{Stdout: []byte("ID_WWN=0x5000C500DEADBEEF\n")})
	if _, err := probeEnclosureIdentity(context.Background(), filesystem, runner, "/dev/sg2"); err == nil || !strings.Contains(err.Error(), "changed during probe") {
		t.Fatalf("changed SES character device accepted: %v", err)
	}
}
