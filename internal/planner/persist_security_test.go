// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadPlanRejectsSymlinkAndSharedWritableFile(t *testing.T) {
	snapshot := createSnapshot()
	plan, err := testBuilder().CreatePool(CreatePoolRequest{
		Snapshot: snapshot, PoolName: "secure", Topology: TopologyMirror,
		Drives: snapshot.Drives[:2],
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("plan blockers=%v err=%v", plan.Blockers, err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := SavePlanAt(path, plan, fixedNow); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlanAt(link, fixedNow); err == nil {
		t.Fatal("symlink plan was accepted")
	}
	if err := os.Chmod(path, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlanAt(path, fixedNow); err == nil || !strings.Contains(err.Error(), "writable by group") {
		t.Fatalf("shared-writable plan error=%v", err)
	}
}

func TestValidatePlanRejectsFutureDatedLifetime(t *testing.T) {
	snapshot := createSnapshot()
	plan, err := testBuilder().CreatePool(CreatePoolRequest{
		Snapshot: snapshot, PoolName: "future", Topology: TopologyMirror, Drives: snapshot.Drives[:2],
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.CreatedAt = fixedNow.Add(24 * time.Hour)
	plan.ExpiresAt = plan.CreatedAt.Add(MaxPlanTTL)
	if err := ValidatePlan(plan, fixedNow); !errors.Is(err, ErrInvalidPlan) || !strings.Contains(err.Error(), "future") {
		t.Fatalf("future-dated plan error=%v", err)
	}
}

func TestValidatePlanRejectsExecutablePlanIDText(t *testing.T) {
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().Maintenance(OperationScrubStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("maintenance plan blockers=%v err=%v", plan.Blockers, err)
	}
	plan.ID = "$(touch /tmp/not-allowed)"
	if err := ValidatePlan(plan, fixedNow); !errors.Is(err, ErrInvalidPlan) || !strings.Contains(err.Error(), "plan id") {
		t.Fatalf("executable plan id error=%v", err)
	}
}
