// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

func TestDriveRefsPinAndHashSectorGeometry(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.TargetDrives) != 1 {
		t.Fatalf("target refs = %#v", plan.TargetDrives)
	}
	ref := plan.TargetDrives[0]
	if ref.LogicalSectorBytes != snapshot.Drives[0].LogicalSectorBytes || ref.PhysicalSectorBytes != snapshot.Drives[0].PhysicalSectorBytes {
		t.Fatalf("pinned geometry = %d/%d, want %d/%d", ref.LogicalSectorBytes, ref.PhysicalSectorBytes, snapshot.Drives[0].LogicalSectorBytes, snapshot.Drives[0].PhysicalSectorBytes)
	}

	changed := plan
	changed.TargetDrives = append([]domain.DriveRef(nil), plan.TargetDrives...)
	changed.TargetDrives[0].LogicalSectorBytes = 4096
	changed.TargetDrives[0].PhysicalSectorBytes = 4096
	if Fingerprint(changed) == Fingerprint(plan) {
		t.Fatal("plan fingerprint ignored pinned sector geometry")
	}

	boot := bootSnapshot()
	bootPlan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: boot, Pool: boot.Pools[0], OldVDev: boot.Pools[0].VDevs[0].Children[1],
		SourceDrive: boot.Drives[0], NewDrive: boot.Drives[2], BootMode: BootModeGRUB,
	})
	if err != nil || len(bootPlan.Blockers) != 0 {
		t.Fatalf("boot plan blockers=%v err=%v", bootPlan.Blockers, err)
	}
	if len(bootPlan.SourceDrives) != 1 || bootPlan.SourceDrives[0].LogicalSectorBytes != 512 || bootPlan.SourceDrives[0].PhysicalSectorBytes != 4096 {
		t.Fatalf("boot source geometry was not pinned: %#v", bootPlan.SourceDrives)
	}
}

func TestRebuildCommandsRejectsHostileDriveRefGeometry(t *testing.T) {
	t.Parallel()
	t.Run("target", func(t *testing.T) {
		snapshot := createSnapshot()
		plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
		if err != nil {
			t.Fatal(err)
		}
		plan.TargetDrives[0].LogicalSectorBytes = 0
		if _, err := RebuildCommands(plan); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("RebuildCommands() error = %v, want ErrInvalidPlan", err)
		}
	})

	t.Run("source", func(t *testing.T) {
		snapshot := bootSnapshot()
		plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
			Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
			SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
		})
		if err != nil {
			t.Fatal(err)
		}
		plan.SourceDrives[0].LogicalSectorBytes = 768
		if _, err := RebuildCommands(plan); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("RebuildCommands() error = %v, want ErrInvalidPlan", err)
		}
	})
}

func TestApplyRejectsTargetSectorGeometryDriftBeforeDispatch(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil {
		t.Fatal(err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	freshCalls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		freshCalls++
		if freshCalls == 2 {
			changed := snapshot
			changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
			changed.Drives[0].LogicalSectorBytes = 4096
			changed.Drives[0].PhysicalSectorBytes = 4096
			return changed, nil
		}
		return snapshot, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "sector geometry changed") {
		t.Fatalf("Apply() error = %v, want sector-geometry revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("geometry drift dispatched calls: %#v", runner.Calls())
	}
}

func TestDiskConsumingPlansRejectKnownFailedTargets(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	failed := false
	snapshot.Drives[2].Health = domain.DriveHealth{
		SMARTPassed: &failed,
		Severity:    domain.SeverityCritical,
		FreshAt:     fixedNow,
	}
	snapshot.Findings = append(snapshot.Findings, domain.Finding{
		ID: "drive:wwn:5000c5000000000c:smart", Scope: "drive/wwn:5000c5000000000c",
		Severity: domain.SeverityCritical, Title: "SMART reports failure",
	})

	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0],
		OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Blockers, " ")
	if !strings.Contains(joined, "failed SMART") || !strings.Contains(joined, "critical health finding") {
		t.Fatalf("known-failed replacement blockers=%v", plan.Blockers)
	}

	// prepare-disk remains usable for explicitly retiring or clearing a failed
	// disk; it does not consume that hardware as a new pool member.
	prepare, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[2]})
	if err != nil || len(prepare.Blockers) != 0 {
		t.Fatalf("prepare failed hardware blockers=%v err=%v", prepare.Blockers, err)
	}
}

func TestApplyRejectsTargetHealthFailureBeforeDispatch(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0],
		OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2],
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("base plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	freshCalls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		freshCalls++
		if freshCalls == 2 {
			changed := snapshot
			changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
			failed := false
			changed.Drives[2].Health = domain.DriveHealth{
				SMARTPassed: &failed, Severity: domain.SeverityCritical, FreshAt: fixedNow,
			}
			return changed, nil
		}
		return snapshot, nil
	}

	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "failed SMART") {
		t.Fatalf("Apply() error=%v, want target-health revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("failed target dispatched calls: %#v", runner.Calls())
	}
}

func TestDiskPlansRequireVDevSizeAuthority(t *testing.T) {
	t.Parallel()

	mirror := mirrorSnapshot()
	mirror.Pools[0].VDevs[0].Children[0].SizeBytes = 0
	replace, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: mirror, Pool: mirror.Pools[0],
		OldVDev: mirror.Pools[0].VDevs[0].Children[0], NewDrive: mirror.Drives[2],
	})
	if err != nil || !strings.Contains(strings.Join(replace.Blockers, " "), "size authority") {
		t.Fatalf("replace blockers=%v err=%v", replace.Blockers, err)
	}
	attach, err := testBuilder().AttachMirror(AttachMirrorRequest{
		Snapshot: mirror, Pool: mirror.Pools[0],
		ExistingVDev: mirror.Pools[0].VDevs[0].Children[0], NewDrive: mirror.Drives[2],
	})
	if err != nil || !strings.Contains(strings.Join(attach.Blockers, " "), "size authority") {
		t.Fatalf("attach blockers=%v err=%v", attach.Blockers, err)
	}

	raidz := raidzSnapshot()
	raidz.Pools[0].VDevs[0].Children[1].SizeBytes = 0
	expand, err := testBuilder().ExpandRAIDZ(ExpandRAIDZRequest{
		Snapshot: raidz, Pool: raidz.Pools[0],
		RAIDZVDev: raidz.Pools[0].VDevs[0], NewDrive: raidz.Drives[3],
	})
	if err != nil || !strings.Contains(strings.Join(expand.Blockers, " "), "child-size authority") {
		t.Fatalf("expand blockers=%v err=%v", expand.Blockers, err)
	}
}

func TestSpareMustCoverLargestDataLeaf(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	snapshot.Drives[2].CapacityBytes = 8 * tebibyte
	tooSmall, err := testBuilder().AddSpare(AddSpareRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], Drives: []domain.Drive{snapshot.Drives[2]},
	})
	if err != nil || !strings.Contains(strings.Join(tooSmall.Blockers, " "), "too small for the pool's largest data leaf") {
		t.Fatalf("small-spare blockers=%v err=%v", tooSmall.Blockers, err)
	}

	unknown := mirrorSnapshot()
	unknown.Pools[0].VDevs[0].Children[1].SizeBytes = 0
	missingSize, err := testBuilder().AddSpare(AddSpareRequest{
		Snapshot: unknown, Pool: unknown.Pools[0], Drives: []domain.Drive{unknown.Drives[2]},
	})
	if err != nil || !strings.Contains(strings.Join(missingSize.Blockers, " "), "spare suitability cannot be proven") {
		t.Fatalf("unknown-size spare blockers=%v err=%v", missingSize.Blockers, err)
	}
}

func TestDiskPlanBlocksConflictingBayMapping(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	snapshot.Drives[2].Bay = domain.Bay{Label: "FRONT-03", Confidence: "conflict"}
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0],
		OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2],
	})
	if err != nil || !strings.Contains(strings.Join(plan.Blockers, " "), "conflicting physical bay mapping") {
		t.Fatalf("bay-conflict blockers=%v err=%v", plan.Blockers, err)
	}
}

func TestApplyRejectsChangedBayMappingBeforeDispatch(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	snapshot.Drives[2].Bay = domain.Bay{Label: "FRONT-03", Confidence: "verified"}
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0],
		OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2],
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("base plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		changed := snapshot
		changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
		if calls > 1 {
			changed.Drives[2].Bay.Label = "REAR-07"
		}
		return changed, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "bay mapping changed") {
		t.Fatalf("Apply() error=%v, want bay revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("changed bay dispatched calls: %#v", runner.Calls())
	}
}

func TestApplyRejectsLostVDevSizeAuthorityBeforeDispatch(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0],
		OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2],
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("base plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	freshCalls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		freshCalls++
		if freshCalls == 2 {
			changed := snapshot
			changed.Pools = append([]domain.Pool(nil), snapshot.Pools...)
			changed.Pools[0].VDevs = append([]domain.VDev(nil), snapshot.Pools[0].VDevs...)
			changed.Pools[0].VDevs[0].Children = append([]domain.VDev(nil), snapshot.Pools[0].VDevs[0].Children...)
			changed.Pools[0].VDevs[0].Children[0].SizeBytes = 0
			return changed, nil
		}
		return snapshot, nil
	}

	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "size authority") {
		t.Fatalf("Apply() error=%v, want size-authority revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("missing vdev size dispatched calls: %#v", runner.Calls())
	}
}

func TestApplyRejectsBootSourceSectorGeometryDriftBeforeDispatch(t *testing.T) {
	t.Parallel()
	snapshot := bootSnapshot()
	plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("boot plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	freshCalls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		freshCalls++
		if freshCalls == 2 {
			changed := snapshot
			changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
			changed.Drives[0].LogicalSectorBytes = 768
			return changed, nil
		}
		return snapshot, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "sector geometry") {
		t.Fatalf("Apply() error = %v, want source geometry revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("source geometry drift dispatched calls: %#v", runner.Calls())
	}
}

func TestApplyRepeatsAddVDevMixedCapacityValidation(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().AddVDev(AddVDevRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], Topology: TopologyMirror,
		Drives: []domain.Drive{snapshot.Drives[2], snapshot.Drives[3]},
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("add-vdev plan blockers=%v err=%v", plan.Blockers, err)
	}

	// Model a handcrafted persisted plan whose drive reference and claimed
	// snapshot fingerprint were both rewritten to hide the builder-time check.
	changed := snapshot
	changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
	changed.Drives[3].CapacityBytes = 6 * tebibyte
	plan.TargetDrives = append([]domain.DriveRef(nil), plan.TargetDrives...)
	plan.TargetDrives[1].CapacityBytes = changed.Drives[3].CapacityBytes
	plan.SnapshotFingerprint = SnapshotFingerprint(changed)
	plan.Commands, err = RebuildCommands(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Confirmation = expectedRedConfirmation(plan)

	runner := execx.NewFakeRunner(false)
	_, err = Apply(context.Background(), plan, applyOptions(changed, runner))
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "capacities differ by more than 1%") {
		t.Fatalf("Apply() error = %v, want mixed-capacity revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("mixed-capacity plan dispatched calls: %#v", runner.Calls())
	}
}

func TestApplyRejectsHandcraftedTargetThatIsSelectedVDevMember(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		build      func(domain.Snapshot) (domain.ActionPlan, error)
		snapshot   func() domain.Snapshot
		member     int
		wantReason string
	}{
		{
			name: "replace", snapshot: mirrorSnapshot, member: 0,
			build: func(snapshot domain.Snapshot) (domain.ActionPlan, error) {
				return testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
			},
			wantReason: "same stable device as the old vdev",
		},
		{
			name: "attach", snapshot: mirrorSnapshot, member: 0,
			build: func(snapshot domain.Snapshot) (domain.ActionPlan, error) {
				return testBuilder().AttachMirror(AttachMirrorRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], ExistingVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
			},
			wantReason: "same stable device as the existing vdev",
		},
		{
			name: "expand", snapshot: raidzSnapshot, member: 0,
			build: func(snapshot domain.Snapshot) (domain.ActionPlan, error) {
				return testBuilder().ExpandRAIDZ(ExpandRAIDZRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], RAIDZVDev: snapshot.Pools[0].VDevs[0], NewDrive: snapshot.Drives[3]})
			},
			wantReason: "already a member of the selected RAIDZ vdev",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			snapshot := test.snapshot()
			plan, err := test.build(snapshot)
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("base plan blockers=%v err=%v", plan.Blockers, err)
			}
			memberRef, reasons := safeDriveRef(snapshot, snapshot.Drives[test.member], false)
			if len(reasons) != 0 {
				t.Fatalf("member reference blockers: %v", reasons)
			}
			plan.TargetDrives = []domain.DriveRef{memberRef}
			plan.Commands, err = RebuildCommands(plan)
			if err != nil {
				t.Fatal(err)
			}
			plan.Confirmation = expectedRedConfirmation(plan)

			runner := execx.NewFakeRunner(false)
			_, err = Apply(context.Background(), plan, applyOptions(snapshot, runner))
			if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("Apply() error = %v, want explicit same-device rejection", err)
			}
			if len(runner.Calls()) != 0 {
				t.Fatalf("same-device plan dispatched calls: %#v", runner.Calls())
			}
		})
	}
}

func TestApplyRepeatsBootOldVDevLeafValidation(t *testing.T) {
	t.Parallel()
	snapshot := bootSnapshot()
	plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("boot plan blockers=%v err=%v", plan.Blockers, err)
	}

	changed := snapshot
	changed.Pools = append([]domain.Pool(nil), snapshot.Pools...)
	changed.Pools[0].VDevs = append([]domain.VDev(nil), snapshot.Pools[0].VDevs...)
	changed.Pools[0].VDevs[0].Children = append([]domain.VDev(nil), snapshot.Pools[0].VDevs[0].Children...)
	changed.Pools[0].VDevs[0].Children[1].Children = []domain.VDev{{Name: "unexpected-child", GUID: "unexpected-child-guid", Type: "disk", State: "ONLINE"}}
	plan.TopologyFingerprint = TopologyFingerprint(changed)
	plan.SnapshotFingerprint = SnapshotFingerprint(changed)
	plan.Confirmation = expectedRedConfirmation(plan)

	runner := execx.NewFakeRunner(false)
	_, err = Apply(context.Background(), plan, applyOptions(changed, runner))
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "old boot vdev") || !strings.Contains(err.Error(), "no longer a leaf") {
		t.Fatalf("Apply() error = %v, want fresh old-vdev leaf rejection", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("non-leaf old-vdev plan dispatched calls: %#v", runner.Calls())
	}
}
