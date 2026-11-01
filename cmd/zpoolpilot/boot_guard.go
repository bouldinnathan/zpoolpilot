// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

// diskGuardFileSystem is deliberately small so the last-mile checks can be
// exercised against adversarial link and sysfs state without requiring test
// block devices. Production always uses osDiskGuardFileSystem.
type diskGuardFileSystem interface {
	Lstat(string) (os.FileInfo, error)
	Readlink(string) (string, error)
	Stat(string) (os.FileInfo, error)
	EvalSymlinks(string) (string, error)
	ReadFile(string) ([]byte, error)
}

type osDiskGuardFileSystem struct{}

func (osDiskGuardFileSystem) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }
func (osDiskGuardFileSystem) Readlink(name string) (string, error)   { return os.Readlink(name) }
func (osDiskGuardFileSystem) Stat(name string) (os.FileInfo, error)  { return os.Stat(name) }
func (osDiskGuardFileSystem) EvalSymlinks(name string) (string, error) {
	return filepath.EvalSymlinks(name)
}
func (osDiskGuardFileSystem) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

type guardedPartition struct {
	drive  domain.DriveRef
	number int
}

type commandDiskGuard struct {
	wholeDisks []domain.DriveRef
	partitions []guardedPartition
}

// guardPlannedCommand binds every destructive command-line disk path to its
// persisted DriveRef immediately before dispatch. For Proxmox boot replacement
// it also proves that fixed partitions 2 and 3 still belong to the planned
// replacement whole disk after partprobe.
func guardPlannedCommand(ctx context.Context, plan domain.ActionPlan, command domain.PlannedCommand, index int) error {
	return guardPlannedCommandWithFS(ctx, osDiskGuardFileSystem{}, plan, command, index)
}

func guardPlannedCommandWithFS(ctx context.Context, fsys diskGuardFileSystem, plan domain.ActionPlan, command domain.PlannedCommand, index int) error {
	if ctx == nil {
		return errors.New("disk-path command guard requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fsys == nil {
		return errors.New("disk-path command guard requires filesystem authority")
	}
	guard, err := diskGuardForCommand(plan, command, index)
	if err != nil {
		return err
	}
	for _, drive := range guard.wholeDisks {
		if err := verifyWholeDisk(fsys, drive); err != nil {
			return err
		}
	}
	for _, partition := range guard.partitions {
		if err := verifyPartitionOwnership(fsys, partition.drive, partition.number); err != nil {
			return err
		}
	}
	return nil
}

func diskGuardForCommand(plan domain.ActionPlan, command domain.PlannedCommand, index int) (commandDiskGuard, error) {
	operation := planner.Operation(plan.Operation)
	switch operation {
	case planner.OperationScrubStart, planner.OperationScrubStop, planner.OperationTrimStart, planner.OperationTrimStop, planner.OperationClearErrors:
		if command.Risk == domain.RiskDestructive {
			return commandDiskGuard{}, fmt.Errorf("routine operation %q unexpectedly contains a destructive command", operation)
		}
		return commandDiskGuard{}, nil
	case planner.OperationCreatePool, planner.OperationPrepareDisk, planner.OperationReplaceDisk,
		planner.OperationAttachMirror, planner.OperationExpandRAIDZ, planner.OperationAddVDev,
		planner.OperationAddSpare, planner.OperationReplaceBootDisk:
		if command.Risk != domain.RiskDestructive {
			return commandDiskGuard{}, fmt.Errorf("destructive operation %q contains command risk %q", operation, command.Risk)
		}
	default:
		return commandDiskGuard{}, fmt.Errorf("operation %q has no disk-path guard", operation)
	}

	targets := plan.TargetDrives
	requireTargets := func(count int) error {
		if len(targets) != count {
			return fmt.Errorf("%s command guard requires exactly %d target drive reference(s), got %d", operation, count, len(targets))
		}
		return nil
	}
	requireOneCommand := func() error {
		if index != 1 {
			return fmt.Errorf("%s has no guarded command %d", operation, index)
		}
		return nil
	}

	switch operation {
	case planner.OperationCreatePool:
		if err := requireOneCommand(); err != nil {
			return commandDiskGuard{}, err
		}
		if len(targets) == 0 || command.Program != "zpool" || len(command.Args) < 2 || command.Args[0] != "create" {
			return commandDiskGuard{}, errors.New("create-pool guard received an unexpected command shape")
		}
		if err := requireDriveTail(command.Args, targets); err != nil {
			return commandDiskGuard{}, fmt.Errorf("create-pool: %w", err)
		}
		return commandDiskGuard{wholeDisks: targets}, nil

	case planner.OperationPrepareDisk:
		if err := requireTargets(1); err != nil {
			return commandDiskGuard{}, err
		}
		path := targets[0].ByID
		valid := index == 1 && command.Program == "sgdisk" && equalStrings(command.Args, []string{"--zap-all", path}) ||
			index == 2 && command.Program == "wipefs" && equalStrings(command.Args, []string{"--all", path})
		if !valid {
			return commandDiskGuard{}, fmt.Errorf("prepare-disk guard received unexpected command %d", index)
		}
		return commandDiskGuard{wholeDisks: targets}, nil

	case planner.OperationReplaceDisk:
		if err := requireTargets(1); err != nil {
			return commandDiskGuard{}, err
		}
		if err := requireOneCommand(); err != nil {
			return commandDiskGuard{}, err
		}
		want := []string{"replace", plan.TargetPool.Name, plan.TargetVDev.GUID, targets[0].ByID}
		if command.Program != "zpool" || !equalStrings(command.Args, want) {
			return commandDiskGuard{}, errors.New("replace-disk guard received an unexpected command shape")
		}
		return commandDiskGuard{wholeDisks: targets}, nil

	case planner.OperationAttachMirror, planner.OperationExpandRAIDZ:
		if err := requireTargets(1); err != nil {
			return commandDiskGuard{}, err
		}
		if err := requireOneCommand(); err != nil {
			return commandDiskGuard{}, err
		}
		want := []string{"attach", plan.TargetPool.Name, plan.TargetVDev.GUID, targets[0].ByID}
		if command.Program != "zpool" || !equalStrings(command.Args, want) {
			return commandDiskGuard{}, fmt.Errorf("%s guard received an unexpected command shape", operation)
		}
		return commandDiskGuard{wholeDisks: targets}, nil

	case planner.OperationAddVDev:
		if err := requireOneCommand(); err != nil {
			return commandDiskGuard{}, err
		}
		if len(targets) == 0 || command.Program != "zpool" || len(command.Args) < 3 || command.Args[0] != "add" || command.Args[1] != plan.TargetPool.Name {
			return commandDiskGuard{}, errors.New("add-vdev guard received an unexpected command shape")
		}
		if err := requireDriveTail(command.Args, targets); err != nil {
			return commandDiskGuard{}, fmt.Errorf("add-vdev: %w", err)
		}
		return commandDiskGuard{wholeDisks: targets}, nil

	case planner.OperationAddSpare:
		if err := requireOneCommand(); err != nil {
			return commandDiskGuard{}, err
		}
		if len(targets) == 0 || command.Program != "zpool" || len(command.Args) != 3+len(targets) ||
			command.Args[0] != "add" || command.Args[1] != plan.TargetPool.Name || command.Args[2] != "spare" {
			return commandDiskGuard{}, errors.New("add-spare guard received an unexpected command shape")
		}
		if err := requireDriveTail(command.Args, targets); err != nil {
			return commandDiskGuard{}, fmt.Errorf("add-spare: %w", err)
		}
		return commandDiskGuard{wholeDisks: targets}, nil

	case planner.OperationReplaceBootDisk:
		return bootDiskGuardForCommand(plan, command, index)
	}
	panic("unreachable")
}

func bootDiskGuardForCommand(plan domain.ActionPlan, command domain.PlannedCommand, index int) (commandDiskGuard, error) {
	if len(plan.SourceDrives) != 1 || len(plan.TargetDrives) != 1 {
		return commandDiskGuard{}, errors.New("boot command guard requires exactly one source and one target drive")
	}
	source, target := plan.SourceDrives[0], plan.TargetDrives[0]
	esp := target.ByID + "-part" + strconv.Itoa(planner.ProxmoxESPPartition)
	zfsPart := target.ByID + "-part" + strconv.Itoa(planner.ProxmoxZFSPartition)
	partitions := []guardedPartition{
		{drive: target, number: planner.ProxmoxESPPartition},
		{drive: target, number: planner.ProxmoxZFSPartition},
	}
	switch index {
	case 1:
		if command.Program != "sgdisk" || !equalStrings(command.Args, []string{source.ByID, "-R", target.ByID}) {
			return commandDiskGuard{}, errors.New("boot partition-clone guard received an unexpected command shape")
		}
		return commandDiskGuard{wholeDisks: []domain.DriveRef{source, target}}, nil
	case 2:
		if command.Program != "sgdisk" || !equalStrings(command.Args, []string{"-G", target.ByID}) {
			return commandDiskGuard{}, errors.New("boot GUID-randomization guard received an unexpected command shape")
		}
		return commandDiskGuard{wholeDisks: []domain.DriveRef{target}}, nil
	case 3:
		if command.Program != "partprobe" || !equalStrings(command.Args, []string{target.ByID}) {
			return commandDiskGuard{}, errors.New("boot partprobe guard received an unexpected command shape")
		}
		return commandDiskGuard{wholeDisks: []domain.DriveRef{target}}, nil
	case 4:
		if command.Program != "proxmox-boot-tool" || !equalStrings(command.Args, []string{"format", esp}) {
			return commandDiskGuard{}, errors.New("boot ESP-format guard received an unexpected command shape")
		}
	case 5:
		want := []string{"init", esp}
		if mode, _ := plan.Desired["boot_mode"].(string); mode == string(planner.BootModeGRUB) {
			want = append(want, "grub")
		} else if mode != string(planner.BootModeSystemdBoot) {
			return commandDiskGuard{}, fmt.Errorf("boot command guard received unsupported boot mode %q", mode)
		}
		if command.Program != "proxmox-boot-tool" || !equalStrings(command.Args, want) {
			return commandDiskGuard{}, errors.New("boot ESP-init guard received an unexpected command shape")
		}
	case 6:
		want := []string{"replace", plan.TargetPool.Name, plan.TargetVDev.GUID, zfsPart}
		if command.Program != "zpool" || !equalStrings(command.Args, want) {
			return commandDiskGuard{}, errors.New("boot ZFS-replacement guard received an unexpected command shape")
		}
	default:
		return commandDiskGuard{}, fmt.Errorf("boot replacement has no guarded command %d", index)
	}
	return commandDiskGuard{wholeDisks: []domain.DriveRef{target}, partitions: partitions}, nil
}

func requireDriveTail(args []string, refs []domain.DriveRef) error {
	if len(args) < len(refs) {
		return errors.New("command has fewer arguments than pinned drive references")
	}
	offset := len(args) - len(refs)
	for index, ref := range refs {
		if args[offset+index] != ref.ByID {
			return fmt.Errorf("disk argument %q does not match pinned drive %q path %q", args[offset+index], ref.ID, ref.ByID)
		}
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type inspectedBlockLink struct {
	majorMinor string
	sysfs      string
}

func verifyWholeDisk(fsys diskGuardFileSystem, drive domain.DriveRef) error {
	if !wholeDiskByIDForGuard(drive.ByID) {
		return fmt.Errorf("planned drive %q is not a whole-disk /dev/disk/by-id path", drive.ByID)
	}
	inspection, err := inspectStableBlockLink(fsys, drive.ByID)
	if err != nil {
		return err
	}
	if inspection.majorMinor != drive.MajorMinor {
		return fmt.Errorf("planned drive %q resolves to %s, not pinned device %s", drive.ByID, inspection.majorMinor, drive.MajorMinor)
	}
	device, err := fsys.ReadFile(filepath.Join(inspection.sysfs, "dev"))
	if err != nil {
		return fmt.Errorf("read sysfs device identity for %q: %w", drive.ByID, err)
	}
	if strings.TrimSpace(string(device)) != drive.MajorMinor {
		return fmt.Errorf("sysfs device for %q is %s, not pinned device %s", drive.ByID, strings.TrimSpace(string(device)), drive.MajorMinor)
	}
	if partition, err := fsys.ReadFile(filepath.Join(inspection.sysfs, "partition")); err == nil {
		return fmt.Errorf("planned whole-disk path %q resolves to kernel partition %q", drive.ByID, strings.TrimSpace(string(partition)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify that %q is not a partition: %w", drive.ByID, err)
	}
	return nil
}

func verifyPartitionOwnership(fsys diskGuardFileSystem, target domain.DriveRef, number int) error {
	path := target.ByID + "-part" + strconv.Itoa(number)
	inspection, err := inspectStableBlockLink(fsys, path)
	if err != nil {
		return err
	}
	partitionNumber, err := fsys.ReadFile(filepath.Join(inspection.sysfs, "partition"))
	if err != nil {
		return fmt.Errorf("%q is not kernel partition %d: %w", path, number, err)
	}
	if strings.TrimSpace(string(partitionNumber)) != strconv.Itoa(number) {
		return fmt.Errorf("%q is kernel partition %q, not %d", path, strings.TrimSpace(string(partitionNumber)), number)
	}
	parent := filepath.Dir(inspection.sysfs)
	parentDevice, err := fsys.ReadFile(filepath.Join(parent, "dev"))
	if err != nil {
		return fmt.Errorf("read parent device for %q: %w", path, err)
	}
	if strings.TrimSpace(string(parentDevice)) != target.MajorMinor {
		return fmt.Errorf("%q belongs to whole disk %s, not planned target %s", path, strings.TrimSpace(string(parentDevice)), target.MajorMinor)
	}
	plannedParent, err := fsys.EvalSymlinks(filepath.Join("/sys/dev/block", target.MajorMinor))
	if err != nil {
		return fmt.Errorf("resolve planned parent sysfs device %s: %w", target.MajorMinor, err)
	}
	if filepath.Clean(plannedParent) != filepath.Clean(parent) {
		return fmt.Errorf("%q sysfs parent %q is not the planned whole disk %q", path, parent, plannedParent)
	}
	return nil
}

func inspectStableBlockLink(fsys diskGuardFileSystem, path string) (inspectedBlockLink, error) {
	linkInfo, err := fsys.Lstat(path)
	if err != nil {
		return inspectedBlockLink{}, fmt.Errorf("inspect planned disk path %q: %w", path, err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		return inspectedBlockLink{}, fmt.Errorf("planned disk path %q is not a persistent by-id symlink", path)
	}
	linkBefore, err := fsys.Readlink(path)
	if err != nil {
		return inspectedBlockLink{}, fmt.Errorf("read planned disk link %q: %w", path, err)
	}
	if linkBefore == "" {
		return inspectedBlockLink{}, fmt.Errorf("planned disk link %q has an empty target", path)
	}
	resolvedBefore, err := fsys.EvalSymlinks(path)
	if err != nil {
		return inspectedBlockLink{}, fmt.Errorf("resolve planned disk link %q: %w", path, err)
	}
	deviceInfo, err := fsys.Stat(path)
	if err != nil {
		return inspectedBlockLink{}, fmt.Errorf("stat planned disk path %q: %w", path, err)
	}
	deviceStat, ok := finalInfoSyscallStat(deviceInfo)
	if !ok || deviceStat.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return inspectedBlockLink{}, fmt.Errorf("planned disk path %q does not resolve to a block device", path)
	}
	majorMinor := blockMajorMinor(uint64(deviceStat.Rdev))
	sysfs, err := fsys.EvalSymlinks(filepath.Join("/sys/dev/block", majorMinor))
	if err != nil {
		return inspectedBlockLink{}, fmt.Errorf("resolve sysfs ownership for %q (%s): %w", path, majorMinor, err)
	}
	sysfs = filepath.Clean(sysfs)
	if !strings.HasPrefix(sysfs, "/sys/devices/") {
		return inspectedBlockLink{}, fmt.Errorf("sysfs ownership path for %q is outside /sys/devices", path)
	}

	linkAfter, err := fsys.Readlink(path)
	if err != nil || linkAfter != linkBefore {
		return inspectedBlockLink{}, fmt.Errorf("planned disk link %q changed during validation", path)
	}
	resolvedAfter, err := fsys.EvalSymlinks(path)
	if err != nil || filepath.Clean(resolvedAfter) != filepath.Clean(resolvedBefore) {
		return inspectedBlockLink{}, fmt.Errorf("planned disk path %q resolved differently during validation", path)
	}
	finalLinkInfo, err := fsys.Lstat(path)
	if err != nil || finalLinkInfo.Mode()&os.ModeSymlink == 0 {
		return inspectedBlockLink{}, fmt.Errorf("planned disk path %q stopped being a by-id symlink during validation", path)
	}
	finalInfo, err := fsys.Stat(path)
	finalStat, ok := finalInfoSyscallStat(finalInfo)
	if err != nil || !ok || finalStat.Mode&syscall.S_IFMT != syscall.S_IFBLK || finalStat.Rdev != deviceStat.Rdev {
		return inspectedBlockLink{}, fmt.Errorf("planned disk path %q changed during validation", path)
	}
	return inspectedBlockLink{majorMinor: majorMinor, sysfs: sysfs}, nil
}

func wholeDiskByIDForGuard(path string) bool {
	if path == "" || filepath.Clean(path) != path || !strings.HasPrefix(path, "/dev/disk/by-id/") {
		return false
	}
	base := filepath.Base(path)
	marker := strings.LastIndex(base, "-part")
	if marker < 0 {
		return base != "." && base != string(filepath.Separator)
	}
	suffix := base[marker+len("-part"):]
	return suffix == "" || strings.Trim(suffix, "0123456789") != ""
}

func finalInfoSyscallStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func blockMajorMinor(device uint64) string {
	major := ((device >> 8) & 0xfff) | ((device >> 32) & ^uint64(0xfff))
	minor := (device & 0xff) | ((device >> 12) & ^uint64(0xff))
	return fmt.Sprintf("%d:%d", major, minor)
}
