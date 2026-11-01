// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

func TestMaintenancePlansBuildExactYellowCommands(t *testing.T) {
	t.Parallel()
	supported := true
	tests := []struct {
		operation Operation
		scan      domain.Scan
		trim      domain.TrimStats
		args      []string
	}{
		{OperationScrubStart, domain.Scan{State: "none"}, idleTrim(), []string{"scrub", "tank"}},
		{OperationScrubStop, domain.Scan{Function: "scrub", State: "scanning"}, domain.TrimStats{}, []string{"scrub", "-s", "tank"}},
		{OperationTrimStart, domain.Scan{State: "none"}, domain.TrimStats{State: "untrimmed", Supported: &supported}, []string{"trim", "tank"}},
		{OperationTrimStop, domain.Scan{}, domain.TrimStats{State: "active", Supported: &supported}, []string{"trim", "-s", "tank"}},
		{OperationClearErrors, domain.Scan{}, domain.TrimStats{}, []string{"clear", "tank"}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.operation), func(t *testing.T) {
			t.Parallel()
			snapshot := mirrorSnapshot()
			snapshot.Pools[0].Scan = test.scan
			snapshot.Pools[0].Trim = test.trim
			plan, err := testBuilder().Maintenance(test.operation, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Blockers) != 0 {
				t.Fatalf("blockers = %v", plan.Blockers)
			}
			if plan.Risk != domain.RiskChange || plan.Confirmation != "" {
				t.Fatalf("risk=%q confirmation=%q", plan.Risk, plan.Confirmation)
			}
			if plan.TargetPool.Name != "tank" || plan.TargetPool.GUID != "pool-1" {
				t.Fatalf("target pool = %#v", plan.TargetPool)
			}
			want := domain.PlannedCommand{Program: "zpool", Args: test.args, Risk: domain.RiskChange}
			if len(plan.Commands) != 1 || plan.Commands[0].Program != want.Program ||
				!reflect.DeepEqual(plan.Commands[0].Args, want.Args) || plan.Commands[0].Risk != want.Risk {
				t.Fatalf("command = %#v, want shape %#v", plan.Commands, want)
			}
			if err := ValidatePlan(plan, fixedNow); err != nil {
				t.Fatalf("ValidatePlan() error = %v", err)
			}
			if test.operation == OperationTrimStart && plan.Desired["trim_intent"] != "start" {
				t.Fatalf("trim intent = %#v", plan.Desired["trim_intent"])
			}
		})
	}
}

func TestMaintenanceScanAndPoolStateBlockers(t *testing.T) {
	t.Parallel()
	supported := true
	tests := []struct {
		name      string
		operation Operation
		state     string
		scan      domain.Scan
		trim      domain.TrimStats
		want      string
	}{
		{"scrub start during resilver", OperationScrubStart, "ONLINE", domain.Scan{Function: "resilver", State: "scanning"}, domain.TrimStats{}, "active resilver"},
		{"scrub start during trim", OperationScrubStart, "ONLINE", domain.Scan{State: "none"}, domain.TrimStats{State: "active", Supported: &supported}, "active manual TRIM"},
		{"scrub start with suspended trim", OperationScrubStart, "ONLINE", domain.Scan{State: "none"}, domain.TrimStats{State: "suspended", Supported: &supported}, "suspended manual TRIM"},
		{"scrub start with unknown trim", OperationScrubStart, "ONLINE", domain.Scan{State: "none"}, domain.TrimStats{State: "unknown"}, "TRIM activity is unknown"},
		{"trim start during scrub", OperationTrimStart, "ONLINE", domain.Scan{Function: "scrub", State: "scanning"}, domain.TrimStats{State: "untrimmed", Supported: &supported}, "active scrub"},
		{"scrub stop with no scrub", OperationScrubStop, "ONLINE", domain.Scan{}, domain.TrimStats{}, "does not have a confirmed active scrub"},
		{"trim stop during scrub", OperationTrimStop, "ONLINE", domain.Scan{Function: "scrub", State: "scanning"}, domain.TrimStats{State: "untrimmed", Supported: &supported}, "does not have a confirmed active manual TRIM"},
		{"unknown trim start", OperationTrimStart, "ONLINE", domain.Scan{}, domain.TrimStats{State: "unknown"}, "consistently known"},
		{"mixed trim start", OperationTrimStart, "ONLINE", domain.Scan{}, domain.TrimStats{State: "mixed"}, "consistently known"},
		{"already active trim", OperationTrimStart, "ONLINE", domain.Scan{}, domain.TrimStats{State: "active", Supported: &supported}, "already has an active manual TRIM"},
		{"suspended pool", OperationClearErrors, "SUSPENDED", domain.Scan{}, domain.TrimStats{}, "not safe for routine maintenance"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := mirrorSnapshot()
			snapshot.Pools[0].State = test.state
			snapshot.Pools[0].Scan = test.scan
			snapshot.Pools[0].Trim = test.trim
			plan, err := testBuilder().Maintenance(test.operation, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(plan.Blockers, " "), test.want) {
				t.Fatalf("blockers %v do not contain %q", plan.Blockers, test.want)
			}
		})
	}
}

func TestTrimStartResumesConfirmedSuspendedPass(t *testing.T) {
	t.Parallel()
	supported := true
	snapshot := mirrorSnapshot()
	snapshot.Pools[0].Trim = domain.TrimStats{State: "suspended", Supported: &supported, Percent: 42}
	plan, err := testBuilder().Maintenance(OperationTrimStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("resume blockers=%v err=%v", plan.Blockers, err)
	}
	if !strings.Contains(plan.ExpectedResult, "resumes") || !strings.Contains(strings.Join(plan.Warnings, " "), "resumes") {
		t.Fatalf("resume result=%q warnings=%v", plan.ExpectedResult, plan.Warnings)
	}
	if plan.Desired["trim_intent"] != "resume" {
		t.Fatalf("resume intent = %#v", plan.Desired["trim_intent"])
	}
}

func TestMaintenanceBootPoolWarnsButIsNotBlocked(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	snapshot.Pools[0].BootCritical = true
	plan, err := testBuilder().Maintenance(OperationClearErrors, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("boot maintenance blockers=%v err=%v", plan.Blockers, err)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "boot-critical") {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
}

func TestMaintenanceApplyUsesYellowConfirmationAndFreshScanCheck(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().Maintenance(OperationScrubStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	if err != nil {
		t.Fatal(err)
	}
	runner := execx.NewFakeRunner(false, execx.FakeResponse{})
	options := applyOptions(snapshot, runner)
	yellow, red := 0, 0
	options.Confirm.Yellow = func(_ context.Context, request ConfirmationRequest) (string, error) {
		yellow++
		if request.Expected != "[Y/n]" || request.Command.Risk != domain.RiskChange {
			t.Errorf("yellow request = %#v", request)
		}
		return "", nil // Enter accepts the documented default.
	}
	options.Confirm.Red = func(context.Context, ConfirmationRequest) (string, error) {
		red++
		return "", errors.New("red callback must not be used")
	}
	result, err := Apply(context.Background(), plan, options)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if yellow != 1 || red != 0 || len(result.Commands) != 1 {
		t.Fatalf("yellow=%d red=%d commands=%d", yellow, red, len(result.Commands))
	}
	calls := runner.Calls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"scrub", "tank"}) || calls[0].Risk != domain.RiskChange {
		t.Fatalf("runner calls = %#v", calls)
	}
}

func TestMaintenanceApplyRejectsScanStartedAfterPreview(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, _ := testBuilder().Maintenance(OperationScrubStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	options.Confirm.Yellow = func(context.Context, ConfirmationRequest) (string, error) { return "yes", nil }
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		if calls > 1 {
			fresh.Pools[0].Scan = domain.Scan{Function: "resilver", State: "scanning"}
		}
		return fresh, nil
	}
	_, err := Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error = %v, want revalidation", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("command dispatched after scan changed: %#v", runner.Calls())
	}
}

func TestScrubApplyRejectsTrimStartedAfterPreview(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().Maintenance(OperationScrubStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("base scrub plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	options.Confirm.Yellow = func(context.Context, ConfirmationRequest) (string, error) { return "yes", nil }
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		if calls > 1 {
			supported := true
			fresh.Pools[0].Trim = domain.TrimStats{State: "active", Supported: &supported}
		}
		return fresh, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) || !strings.Contains(err.Error(), "active manual TRIM") {
		t.Fatalf("Apply() error=%v, want TRIM conflict revalidation", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("scrub dispatched during active TRIM: %#v", runner.Calls())
	}
}

func TestTrimApplyRejectsLeafStateChangeAfterPreview(t *testing.T) {
	t.Parallel()
	supported := true
	snapshot := mirrorSnapshot()
	snapshot.Pools[0].Trim = domain.TrimStats{State: "untrimmed", Supported: &supported}
	plan, _ := testBuilder().Maintenance(OperationTrimStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	options.Confirm.Yellow = func(context.Context, ConfirmationRequest) (string, error) { return "", nil }
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		if calls > 1 {
			fresh.Pools[0].Trim = domain.TrimStats{State: "active", Supported: &supported}
		}
		return fresh, nil
	}
	_, err := Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error = %v, want revalidation", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("trim dispatched after state changed: %#v", runner.Calls())
	}
}

func TestTrimApplyDoesNotChangeFromResumeToNewPass(t *testing.T) {
	t.Parallel()
	supported := true
	snapshot := mirrorSnapshot()
	snapshot.Pools[0].Trim = domain.TrimStats{State: "suspended", Supported: &supported, Percent: 30}
	plan, _ := testBuilder().Maintenance(OperationTrimStart, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	options.Confirm.Yellow = func(context.Context, ConfirmationRequest) (string, error) { return "", nil }
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		if calls > 1 {
			fresh.Pools[0].Trim = domain.TrimStats{State: "complete", Supported: &supported, Percent: 100}
		}
		return fresh, nil
	}
	_, err := Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error=%v, want revalidation", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("resume plan unexpectedly started a new pass: %#v", runner.Calls())
	}
}

func TestMaintenanceTamperingAndRedRegression(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, _ := testBuilder().Maintenance(OperationClearErrors, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	plan.Commands[0].Args = []string{"clear", "-F", "tank"}
	if err := ValidatePlan(plan, fixedNow); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("command tamper error = %v", err)
	}
	plan, _ = testBuilder().Maintenance(OperationClearErrors, MaintenanceRequest{Snapshot: snapshot, Pool: snapshot.Pools[0]})
	plan.Confirmation = "yes anything"
	if err := ValidatePlan(plan, fixedNow); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("yellow confirmation tamper error = %v", err)
	}
	red, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[2]})
	if red.Risk != domain.RiskDestructive || !strings.HasPrefix(red.Confirmation, "yes ") {
		t.Fatalf("red regression: risk=%q confirmation=%q", red.Risk, red.Confirmation)
	}
}

func TestLifecyclePlansBlockConcurrentPoolActivity(t *testing.T) {
	t.Parallel()
	t.Run("target scan", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		snapshot.Pools[0].Scan = domain.Scan{Function: "resilver", State: "scanning"}
		plan, err := testBuilder().AddSpare(AddSpareRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], Drives: []domain.Drive{snapshot.Drives[2]}})
		if err != nil || !strings.Contains(strings.Join(plan.Blockers, " "), "active resilver") {
			t.Fatalf("blockers=%v err=%v", plan.Blockers, err)
		}
	})
	t.Run("target trim", func(t *testing.T) {
		supported := true
		snapshot := mirrorSnapshot()
		snapshot.Pools[0].Trim = domain.TrimStats{State: "active", Supported: &supported}
		plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
		if err != nil || !strings.Contains(strings.Join(plan.Blockers, " "), "active manual TRIM") {
			t.Fatalf("blockers=%v err=%v", plan.Blockers, err)
		}
	})
	t.Run("unknown scan authority", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		snapshot.Pools[0].Scan = domain.Scan{Function: "scrub", State: "future-state"}
		plan, err := testBuilder().AddSpare(AddSpareRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], Drives: []domain.Drive{snapshot.Drives[2]}})
		if err != nil || !strings.Contains(strings.Join(plan.Blockers, " "), "scan activity is unknown") {
			t.Fatalf("blockers=%v err=%v", plan.Blockers, err)
		}
	})
	t.Run("create while another pool is active", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		snapshot.Pools[0].Scan = domain.Scan{Function: "scrub", State: "scanning"}
		plan, err := testBuilder().CreatePool(CreatePoolRequest{Snapshot: snapshot, PoolName: "newpool", Topology: TopologyMirror, Drives: []domain.Drive{snapshot.Drives[2], snapshot.Drives[3]}})
		if err != nil || !strings.Contains(strings.Join(plan.Blockers, " "), "active scrub") {
			t.Fatalf("blockers=%v err=%v", plan.Blockers, err)
		}
	})
	t.Run("mixed trim with active leaf", func(t *testing.T) {
		supported := true
		snapshot := mirrorSnapshot()
		snapshot.Pools[0].Trim = domain.TrimStats{State: "mixed"}
		snapshot.Pools[0].VDevs[0].Children[0].Trim = domain.TrimStats{State: "active", Supported: &supported}
		snapshot.Pools[0].VDevs[0].Children[1].Trim = domain.TrimStats{State: "complete", Supported: &supported}
		blockers := poolActivityBlockers(snapshot.Pools[0])
		if !strings.Contains(strings.Join(blockers, " "), "one or more leaf") {
			t.Fatalf("blockers=%v", blockers)
		}
	})
}

func TestLifecycleApplyRejectsActivityStartedAfterPreview(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		if calls > 1 {
			fresh.Pools[0].Scan = domain.Scan{Function: "scrub", State: "scanning"}
		}
		return fresh, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error=%v, want revalidation", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("lifecycle command dispatched during active scrub: %#v", runner.Calls())
	}
}

func TestLifecycleApplyRejectsUnknownScanAfterPreview(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		fresh.Pools[0].Scan = domain.Scan{Function: "scrub", State: "future-state"}
		return fresh, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error=%v, want revalidation", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("lifecycle command dispatched with unknown scan authority: %#v", runner.Calls())
	}
}

func TestMultiCommandLifecycleRevalidatesPoolActivityBetweenCommands(t *testing.T) {
	t.Parallel()
	snapshot := bootSnapshot()
	plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false, execx.FakeResponse{})
	options := applyOptions(snapshot, runner)
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		fresh := snapshot
		fresh.Pools = append([]domain.Pool(nil), snapshot.Pools...)
		if calls > 2 {
			fresh.Pools[0].Scan = domain.Scan{Function: "resilver", State: "scanning"}
		}
		return fresh, nil
	}
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error=%v, want revalidation", err)
	}
	if len(runner.Calls()) != 1 {
		t.Fatalf("dispatched %d commands, want only the first before activity appeared", len(runner.Calls()))
	}
}
