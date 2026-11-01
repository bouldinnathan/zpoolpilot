// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

type fakeDiskGuardFileInfo struct {
	name string
	mode os.FileMode
	stat syscall.Stat_t
}

func (info fakeDiskGuardFileInfo) Name() string       { return info.name }
func (info fakeDiskGuardFileInfo) Size() int64        { return 0 }
func (info fakeDiskGuardFileInfo) Mode() os.FileMode  { return info.mode }
func (info fakeDiskGuardFileInfo) ModTime() time.Time { return time.Time{} }
func (info fakeDiskGuardFileInfo) IsDir() bool        { return false }
func (info fakeDiskGuardFileInfo) Sys() any           { return &info.stat }

type fakeDiskGuardFS struct {
	links         map[string]string
	relinked      map[string]string
	readlinkCalls map[string]int
	stats         map[string]syscall.Stat_t
	resolved      map[string]string
	files         map[string][]byte
}

func newFakeDiskGuardFS() *fakeDiskGuardFS {
	return &fakeDiskGuardFS{
		links: make(map[string]string), relinked: make(map[string]string), readlinkCalls: make(map[string]int),
		stats: make(map[string]syscall.Stat_t), resolved: make(map[string]string), files: make(map[string][]byte),
	}
}

func (fsys *fakeDiskGuardFS) Lstat(path string) (os.FileInfo, error) {
	if _, ok := fsys.links[path]; !ok {
		return nil, os.ErrNotExist
	}
	return fakeDiskGuardFileInfo{name: filepath.Base(path), mode: os.ModeSymlink, stat: syscall.Stat_t{Mode: syscall.S_IFLNK}}, nil
}

func (fsys *fakeDiskGuardFS) Readlink(path string) (string, error) {
	link, ok := fsys.links[path]
	if !ok {
		return "", os.ErrNotExist
	}
	fsys.readlinkCalls[path]++
	if changed, ok := fsys.relinked[path]; ok && fsys.readlinkCalls[path] > 1 {
		return changed, nil
	}
	return link, nil
}

func (fsys *fakeDiskGuardFS) Stat(path string) (os.FileInfo, error) {
	stat, ok := fsys.stats[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return fakeDiskGuardFileInfo{name: filepath.Base(path), mode: os.ModeDevice, stat: stat}, nil
}

func (fsys *fakeDiskGuardFS) EvalSymlinks(path string) (string, error) {
	resolved, ok := fsys.resolved[path]
	if !ok {
		return "", os.ErrNotExist
	}
	return resolved, nil
}

func (fsys *fakeDiskGuardFS) ReadFile(path string) ([]byte, error) {
	value, ok := fsys.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func guardDrive(name string, minor uint64) domain.DriveRef {
	return domain.DriveRef{
		ID: "wwn:" + name, WWN: "0x" + name, Serial: "SERIAL-" + strings.ToUpper(name),
		ByID: "/dev/disk/by-id/wwn-" + name, MajorMinor: fmt.Sprintf("8:%d", minor),
		CapacityBytes: 10 << 40, LogicalSectorBytes: 512, PhysicalSectorBytes: 4096,
	}
}

func linuxDeviceNumber(major, minor uint64) uint64 {
	return (major&0xfff)<<8 | (major&^uint64(0xfff))<<32 | (minor & 0xff) | (minor&^uint64(0xff))<<12
}

func addWholeDisk(fsys *fakeDiskGuardFS, drive domain.DriveRef, devName string) string {
	var major, minor uint64
	if _, err := fmt.Sscanf(drive.MajorMinor, "%d:%d", &major, &minor); err != nil {
		panic(err)
	}
	sysfs := "/sys/devices/pci0000:00/block/" + devName
	fsys.links[drive.ByID] = "../../" + devName
	fsys.stats[drive.ByID] = syscall.Stat_t{Mode: syscall.S_IFBLK, Rdev: linuxDeviceNumber(major, minor)}
	fsys.resolved[drive.ByID] = "/dev/" + devName
	fsys.resolved[filepath.Join("/sys/dev/block", drive.MajorMinor)] = sysfs
	fsys.files[filepath.Join(sysfs, "dev")] = []byte(drive.MajorMinor + "\n")
	return sysfs
}

func addPartition(fsys *fakeDiskGuardFS, drive domain.DriveRef, devName string, number int, minor uint64, parent string) string {
	path := fmt.Sprintf("%s-part%d", drive.ByID, number)
	partitionName := fmt.Sprintf("%s%d", devName, number)
	sysfs := filepath.Join(parent, partitionName)
	majorMinor := fmt.Sprintf("8:%d", minor)
	fsys.links[path] = "../../" + partitionName
	fsys.stats[path] = syscall.Stat_t{Mode: syscall.S_IFBLK, Rdev: linuxDeviceNumber(8, minor)}
	fsys.resolved[path] = "/dev/" + partitionName
	fsys.resolved[filepath.Join("/sys/dev/block", majorMinor)] = sysfs
	fsys.files[filepath.Join(sysfs, "partition")] = []byte(fmt.Sprintf("%d\n", number))
	return sysfs
}

func bootGuardPlan(source, target domain.DriveRef) domain.ActionPlan {
	return domain.ActionPlan{
		Operation: string(planner.OperationReplaceBootDisk), Risk: domain.RiskDestructive,
		TargetPool:   domain.PoolRef{Name: "rpool", GUID: "pool-guid"},
		TargetVDev:   domain.VDevRef{Name: "old", GUID: "old-guid"},
		SourceDrives: []domain.DriveRef{source}, TargetDrives: []domain.DriveRef{target},
		Desired: map[string]any{"boot_mode": string(planner.BootModeGRUB)},
	}
}

func TestBootCommandGuardChecksWholeDisksBeforePartprobe(t *testing.T) {
	t.Parallel()
	source, target := guardDrive("source", 16), guardDrive("target", 32)
	fsys := newFakeDiskGuardFS()
	addWholeDisk(fsys, source, "sdb")
	addWholeDisk(fsys, target, "sdc")
	plan := bootGuardPlan(source, target)
	commands := []domain.PlannedCommand{
		{Program: "sgdisk", Args: []string{source.ByID, "-R", target.ByID}, Risk: domain.RiskDestructive},
		{Program: "sgdisk", Args: []string{"-G", target.ByID}, Risk: domain.RiskDestructive},
		{Program: "partprobe", Args: []string{target.ByID}, Risk: domain.RiskDestructive},
	}
	for index, command := range commands {
		if err := guardPlannedCommandWithFS(context.Background(), fsys, plan, command, index+1); err != nil {
			t.Fatalf("command %d guard error: %v", index+1, err)
		}
	}
}

func TestWholeDiskGuardRejectsRelinkWrongDeviceAndPartition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*fakeDiskGuardFS, domain.DriveRef, string)
		want string
	}{
		{
			name: "relinked during validation",
			edit: func(fsys *fakeDiskGuardFS, drive domain.DriveRef, _ string) {
				fsys.relinked[drive.ByID] = "../../sdz"
			},
			want: "changed during validation",
		},
		{
			name: "wrong block device",
			edit: func(fsys *fakeDiskGuardFS, drive domain.DriveRef, _ string) {
				fsys.stats[drive.ByID] = syscall.Stat_t{Mode: syscall.S_IFBLK, Rdev: linuxDeviceNumber(8, 33)}
				otherSysfs := "/sys/devices/pci0000:00/block/sdz"
				fsys.resolved["/sys/dev/block/8:33"] = otherSysfs
				fsys.files[filepath.Join(otherSysfs, "dev")] = []byte("8:33\n")
			},
			want: "not pinned device",
		},
		{
			name: "partition masquerading as whole disk",
			edit: func(fsys *fakeDiskGuardFS, _ domain.DriveRef, sysfs string) {
				fsys.files[filepath.Join(sysfs, "partition")] = []byte("7\n")
			},
			want: "resolves to kernel partition",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target := guardDrive("target", 32)
			fsys := newFakeDiskGuardFS()
			sysfs := addWholeDisk(fsys, target, "sdc")
			test.edit(fsys, target, sysfs)
			plan := domain.ActionPlan{Operation: string(planner.OperationPrepareDisk), Risk: domain.RiskDestructive, TargetDrives: []domain.DriveRef{target}}
			command := domain.PlannedCommand{Program: "sgdisk", Args: []string{"--zap-all", target.ByID}, Risk: domain.RiskDestructive}
			err := guardPlannedCommandWithFS(context.Background(), fsys, plan, command, 1)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("guard error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBootPartitionGuardRetainsFixedParentOwnershipChecks(t *testing.T) {
	t.Parallel()
	source, target := guardDrive("source", 16), guardDrive("target", 32)
	plan := bootGuardPlan(source, target)
	command := domain.PlannedCommand{Program: "proxmox-boot-tool", Args: []string{"format", target.ByID + "-part2"}, Risk: domain.RiskDestructive}

	good := newFakeDiskGuardFS()
	parent := addWholeDisk(good, target, "sdc")
	addPartition(good, target, "sdc", planner.ProxmoxESPPartition, 34, parent)
	addPartition(good, target, "sdc", planner.ProxmoxZFSPartition, 35, parent)
	if err := guardPlannedCommandWithFS(context.Background(), good, plan, command, 4); err != nil {
		t.Fatalf("valid fixed partitions rejected: %v", err)
	}

	wrong := newFakeDiskGuardFS()
	parent = addWholeDisk(wrong, target, "sdc")
	otherParent := "/sys/devices/pci0000:00/block/sdd"
	wrong.files[filepath.Join(otherParent, "dev")] = []byte("8:48\n")
	addPartition(wrong, target, "sdd", planner.ProxmoxESPPartition, 50, otherParent)
	addPartition(wrong, target, "sdc", planner.ProxmoxZFSPartition, 35, parent)
	err := guardPlannedCommandWithFS(context.Background(), wrong, plan, command, 4)
	if err == nil || !strings.Contains(err.Error(), "not planned target") {
		t.Fatalf("wrong partition parent accepted: %v", err)
	}
}

func TestCommandGuardCoversEveryGenericDestructiveDiskPath(t *testing.T) {
	t.Parallel()
	first, second := guardDrive("first", 16), guardDrive("second", 32)
	baseFS := func() *fakeDiskGuardFS {
		fsys := newFakeDiskGuardFS()
		addWholeDisk(fsys, first, "sdb")
		addWholeDisk(fsys, second, "sdc")
		return fsys
	}
	tests := []struct {
		name    string
		plan    domain.ActionPlan
		command domain.PlannedCommand
		index   int
	}{
		{
			name:    "create-pool",
			plan:    domain.ActionPlan{Operation: string(planner.OperationCreatePool), Risk: domain.RiskDestructive, TargetDrives: []domain.DriveRef{first, second}},
			command: domain.PlannedCommand{Program: "zpool", Args: []string{"create", "tank", "mirror", first.ByID, second.ByID}, Risk: domain.RiskDestructive}, index: 1,
		},
		{
			name:    "prepare-disk second command",
			plan:    domain.ActionPlan{Operation: string(planner.OperationPrepareDisk), Risk: domain.RiskDestructive, TargetDrives: []domain.DriveRef{first}},
			command: domain.PlannedCommand{Program: "wipefs", Args: []string{"--all", first.ByID}, Risk: domain.RiskDestructive}, index: 2,
		},
		{
			name:    "replace-disk",
			plan:    domain.ActionPlan{Operation: string(planner.OperationReplaceDisk), Risk: domain.RiskDestructive, TargetPool: domain.PoolRef{Name: "tank"}, TargetVDev: domain.VDevRef{GUID: "leaf-guid"}, TargetDrives: []domain.DriveRef{first}},
			command: domain.PlannedCommand{Program: "zpool", Args: []string{"replace", "tank", "leaf-guid", first.ByID}, Risk: domain.RiskDestructive}, index: 1,
		},
		{
			name:    "attach-mirror",
			plan:    domain.ActionPlan{Operation: string(planner.OperationAttachMirror), Risk: domain.RiskDestructive, TargetPool: domain.PoolRef{Name: "tank"}, TargetVDev: domain.VDevRef{GUID: "leaf-guid"}, TargetDrives: []domain.DriveRef{first}},
			command: domain.PlannedCommand{Program: "zpool", Args: []string{"attach", "tank", "leaf-guid", first.ByID}, Risk: domain.RiskDestructive}, index: 1,
		},
		{
			name:    "expand-raidz",
			plan:    domain.ActionPlan{Operation: string(planner.OperationExpandRAIDZ), Risk: domain.RiskDestructive, TargetPool: domain.PoolRef{Name: "tank"}, TargetVDev: domain.VDevRef{GUID: "raidz-guid"}, TargetDrives: []domain.DriveRef{first}},
			command: domain.PlannedCommand{Program: "zpool", Args: []string{"attach", "tank", "raidz-guid", first.ByID}, Risk: domain.RiskDestructive}, index: 1,
		},
		{
			name:    "add-vdev",
			plan:    domain.ActionPlan{Operation: string(planner.OperationAddVDev), Risk: domain.RiskDestructive, TargetPool: domain.PoolRef{Name: "tank"}, TargetDrives: []domain.DriveRef{first, second}},
			command: domain.PlannedCommand{Program: "zpool", Args: []string{"add", "tank", "mirror", first.ByID, second.ByID}, Risk: domain.RiskDestructive}, index: 1,
		},
		{
			name:    "add-spare",
			plan:    domain.ActionPlan{Operation: string(planner.OperationAddSpare), Risk: domain.RiskDestructive, TargetPool: domain.PoolRef{Name: "tank"}, TargetDrives: []domain.DriveRef{first, second}},
			command: domain.PlannedCommand{Program: "zpool", Args: []string{"add", "tank", "spare", first.ByID, second.ByID}, Risk: domain.RiskDestructive}, index: 1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := guardPlannedCommandWithFS(context.Background(), baseFS(), test.plan, test.command, test.index); err != nil {
				t.Fatalf("guard error: %v", err)
			}
		})
	}
}

func TestCommandGuardRejectsPathNotBoundToPinnedDriveRef(t *testing.T) {
	t.Parallel()
	first, second := guardDrive("first", 16), guardDrive("second", 32)
	fsys := newFakeDiskGuardFS()
	addWholeDisk(fsys, first, "sdb")
	addWholeDisk(fsys, second, "sdc")
	plan := domain.ActionPlan{Operation: string(planner.OperationPrepareDisk), Risk: domain.RiskDestructive, TargetDrives: []domain.DriveRef{first}}
	command := domain.PlannedCommand{Program: "sgdisk", Args: []string{"--zap-all", second.ByID}, Risk: domain.RiskDestructive}
	if err := guardPlannedCommandWithFS(context.Background(), fsys, plan, command, 1); err == nil || !strings.Contains(err.Error(), "unexpected command") {
		t.Fatalf("unbound disk path accepted: %v", err)
	}
}

func TestBlockMajorMinorLinuxEncoding(t *testing.T) {
	if got := blockMajorMinor(uint64(8<<8 | 17)); got != "8:17" {
		t.Fatalf("major:minor=%q, want 8:17", got)
	}
}
