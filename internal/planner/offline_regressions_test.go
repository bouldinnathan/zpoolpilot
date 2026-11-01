// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

func TestLoadPlanRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoadPlanAt(path, fixedNow)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidPlan) || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO plan error = %v", err)
		}
	case <-time.After(time.Second):
		// Unblock the old implementation so a regression cannot leak a reader.
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = syscall.Close(fd)
		}
		t.Fatal("plan loading waited for a FIFO writer before validating its type")
	}
}

func TestPrepareRechecksRawOwnershipBeforeWipingSignatures(t *testing.T) {
	for _, signature := range []string{"zfs_member", "ceph_bluestore", "LVM2_member", "linux_raid_member", "crypto_LUKS", "BitLocker", "swap"} {
		t.Run(signature, func(t *testing.T) {
			snapshot := createSnapshot()
			plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("plan blockers=%v err=%v", plan.Blockers, err)
			}
			runner := execx.NewFakeRunner(false, execx.FakeResponse{}, execx.FakeResponse{})
			options := applyOptions(snapshot, runner)
			freshCalls := 0
			options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
				freshCalls++
				fresh := snapshot
				if freshCalls >= 3 {
					fresh.Drives = append([]domain.Drive(nil), snapshot.Drives...)
					fresh.Drives[0].InUse = true
					fresh.Drives[0].Use = "/dev/sda contains " + signature
				}
				return fresh, nil
			}
			result, err := Apply(context.Background(), plan, options)
			if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "ownership signature") {
				t.Fatalf("Apply() error = %v, want raw ownership rejection", err)
			}
			if calls := runner.Calls(); len(calls) != 1 || calls[0].Program != "sgdisk" || len(result.Commands) != 1 {
				t.Fatalf("unsafe wipe was dispatched: %+v", calls)
			}
		})
	}
}

func TestBootPartitionCloneRequiresMatchingLogicalSectors(t *testing.T) {
	for _, geometry := range [][2]uint64{{512, 4096}, {4096, 512}} {
		snapshot := bootSnapshot()
		snapshot.Drives[0].LogicalSectorBytes = geometry[0]
		snapshot.Drives[2].LogicalSectorBytes = geometry[1]
		plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
			Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
			SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(plan.Blockers, "; "), "logical sector sizes must match") {
			t.Errorf("source/target sectors %v accepted at planning: %v", geometry, plan.Blockers)
		}
		// An edited plan must not bypass the same check at apply time.
		plan.Blockers = nil
		plan.Confirmation = expectedRedConfirmation(plan)
		runner := execx.NewFakeRunner(false)
		_, err = Apply(context.Background(), plan, applyOptions(snapshot, runner))
		if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "logical sector sizes must match") {
			t.Errorf("source/target sectors %v apply error=%v", geometry, err)
		}
		if len(runner.Calls()) != 0 {
			t.Fatalf("incompatible boot partition clone dispatched: %+v", runner.Calls())
		}
	}
}

func TestLoadPlanRejectsDirectory(t *testing.T) {
	_, err := LoadPlanAt(t.TempDir(), fixedNow)
	if !errors.Is(err, ErrInvalidPlan) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory plan error=%v", err)
	}
}

func TestSingleTopologyCannotCreateSeveralTopLevelVDevs(t *testing.T) {
	snapshot := createSnapshot()
	for _, count := range []int{1, 2} {
		plan, err := testBuilder().CreatePool(CreatePoolRequest{
			Snapshot: snapshot, PoolName: "singlepool", Topology: TopologySingle,
			Drives: snapshot.Drives[:count], Expert: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			if len(plan.Blockers) != 0 {
				t.Fatalf("single-drive expert plan blocked: %v", plan.Blockers)
			}
			continue
		}
		if !strings.Contains(strings.Join(plan.Blockers, "; "), "exactly one drive") {
			t.Errorf("single topology accepted a striped pool: %v", plan.Blockers)
		}
		plan.Blockers = nil
		plan.Confirmation = expectedRedConfirmation(plan)
		runner := execx.NewFakeRunner(false)
		_, err = Apply(context.Background(), plan, applyOptions(snapshot, runner))
		if !errors.Is(err, ErrRevalidation) || len(runner.Calls()) != 0 {
			t.Fatalf("edited single topology plan was not rejected before dispatch: error=%v calls=%+v", err, runner.Calls())
		}
	}
}
