// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/config"
	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/monitor"
	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

func TestStatusRefreshAndInventoryScanPreserveCallerContextEnd(t *testing.T) {
	tests := []struct {
		name string
		args []string
		run  func(*app, context.Context, []string) (int, error)
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "status cancellation", args: []string{"--refresh", "--json"},
			run: func(a *app, ctx context.Context, args []string) (int, error) { return a.runStatus(ctx, args) },
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			want: context.Canceled,
		},
		{
			name: "inventory deadline", args: []string{"scan", "--json"},
			run: func(a *app, ctx context.Context, args []string) (int, error) { return a.runInventory(ctx, args) },
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			want: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := cancellationCommandTestApp(t)
			ctx, cancel := test.ctx()
			defer cancel()
			_, err := test.run(a, ctx, test.args)
			if !errors.Is(err, test.want) {
				t.Fatalf("command error = %v, want preserved %v", err, test.want)
			}
			if info, statErr := a.out.Stat(); statErr != nil || info.Size() == 0 {
				t.Fatalf("partial JSON was not rendered: size=%d err=%v", infoSize(info), statErr)
			}
		})
	}
}

func cancellationCommandTestApp(t *testing.T) *app {
	t.Helper()
	directory := t.TempDir()
	out, err := os.Create(filepath.Join(directory, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = out.Close()
		_ = errOut.Close()
	})
	cfg := config.Defaults()
	cfg.SnapshotPath = filepath.Join(directory, "latest.json")
	cfg.StatePath = filepath.Join(directory, "state.db")
	cfg.BayMapPath = filepath.Join(directory, "bays.json")
	cfg.CollectionLockPath = filepath.Join(directory, "collect.lock")
	cfg.CommandTimeout = 50 * time.Millisecond
	return &app{cfg: cfg, readOnly: true, out: out, errOut: errOut}
}

func TestCachedInventoryListPreservesBayMergeCancellation(t *testing.T) {
	a := cancellationCommandTestApp(t)
	wantSnapshot := domain.Snapshot{
		APIVersion: domain.SnapshotAPIVersion,
		Generated:  time.Now().UTC(),
		Pools:      []domain.Pool{}, Datasets: []domain.Dataset{}, Drives: []domain.Drive{},
		Findings: []domain.Finding{}, Errors: []domain.SourceError{},
	}
	if err := monitor.SaveAtomic(a.cfg.SnapshotPath, wantSnapshot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.runInventory(ctx, []string{"list", "--json"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("inventory list error = %v, want preserved context cancellation", err)
	}
}

func infoSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

func TestRecordBayMappingErrorMarksPartialOnce(t *testing.T) {
	snapshot := domain.Snapshot{}
	recordBayMappingError(&snapshot, errors.New("untrusted mapping file"))
	recordBayMappingError(&snapshot, errors.New("second error"))
	if !snapshot.Partial || len(snapshot.Errors) != 1 {
		t.Fatalf("snapshot partial=%t errors=%+v", snapshot.Partial, snapshot.Errors)
	}
	got := snapshot.Errors[0]
	if got.Source != "inventory.bays" || got.Kind != "trust" || !got.Partial || got.Message != "untrusted mapping file" {
		t.Fatalf("bay source error = %+v", got)
	}
}

func TestSafetySnapshotErrorIgnoresTelemetryButBlocksAuthorityLoss(t *testing.T) {
	snapshot := domain.Snapshot{Host: domain.Host{MachineID: "machine"}, Errors: []domain.SourceError{
		{Source: "arcstats", Kind: "not-found", Message: "optional", Partial: true},
		{Source: "smart:disk-a", Kind: "timeout", Message: "optional", Partial: true},
		{Source: "zpool.iostat", Kind: "timeout", Message: "optional", Partial: true},
	}}
	if err := safetySnapshotError(snapshot); err != nil {
		t.Fatalf("optional telemetry blocked planning: %v", err)
	}
	snapshot.Errors = append(snapshot.Errors, domain.SourceError{Source: "udev:/dev/sda", Kind: "permission", Message: "denied", Partial: true})
	if err := safetySnapshotError(snapshot); err == nil || !strings.Contains(err.Error(), "udev:/dev/sda") {
		t.Fatalf("identity loss was not blocked: %v", err)
	}
	snapshot.Errors = []domain.SourceError{{Source: "drive.holders:/dev/sda1", Kind: "permission", Message: "denied", Partial: true}}
	if err := safetySnapshotError(snapshot); err == nil || !strings.Contains(err.Error(), "drive.holders") {
		t.Fatalf("holder authority loss was not blocked: %v", err)
	}
	snapshot.Errors = []domain.SourceError{{Source: "host.root-mount", Kind: "parse", Message: "missing root mount", Partial: true}}
	if err := safetySnapshotError(snapshot); err == nil || !strings.Contains(err.Error(), "host.root-mount") {
		t.Fatalf("root-mount authority loss was not blocked: %v", err)
	}
}

func TestValidateKeyFile(t *testing.T) {
	directory := t.TempDir()
	secure := filepath.Join(directory, "key")
	if err := os.WriteFile(secure, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateKeyFile(secure); err != nil {
		t.Fatalf("secure key rejected: %v", err)
	}
	hardlink := filepath.Join(directory, "key-hardlink")
	if err := os.Link(secure, hardlink); err != nil {
		t.Fatal(err)
	}
	if err := validateKeyFile(secure); err == nil || !strings.Contains(err.Error(), "singly linked") {
		t.Fatalf("multiply linked key accepted: %v", err)
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secure, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateKeyFile(secure); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("exposed key accepted: %v", err)
	}
	symlink := filepath.Join(directory, "key-link")
	if err := os.Symlink(secure, symlink); err != nil {
		t.Fatal(err)
	}
	if err := validateKeyFile(symlink); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink key accepted: %v", err)
	}
}

func TestValidatePlanKeyFileRechecksLoadedRepresentation(t *testing.T) {
	directory := t.TempDir()
	key := filepath.Join(directory, "pool.key")
	if err := os.WriteFile(key, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := domain.ActionPlan{Operation: string(planner.OperationCreatePool), Desired: map[string]any{
		"dataset_properties": map[string]any{
			"encryption": "on", "keyformat": "passphrase", "keylocation": "file://" + key,
		},
	}}
	if err := validatePlanKeyFile(plan); os.Geteuid() == 0 {
		if err == nil || !strings.Contains(err.Error(), "not trusted for privileged use") {
			t.Fatalf("key beneath a writable temporary tree was trusted by root: %v", err)
		}
	} else if err != nil {
		t.Fatalf("safe unprivileged planned keyfile rejected: %v", err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validatePlanKeyFile(plan); err == nil || !strings.Contains(err.Error(), "no longer safe") {
		t.Fatalf("changed planned keyfile accepted: %v", err)
	}
}

func TestDetectProxmoxBootMode(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   planner.BootMode
		bad    bool
	}{
		{"systemd boot", "System currently booted with uefi\nABCD-1234 is configured with: uefi (versions: 6.17)", planner.BootModeSystemdBoot, false},
		{"grub", "System currently booted with legacy bios\nABCD-1234 is configured with: grub (versions: 6.17)", planner.BootModeGRUB, false},
		{"mixed", "A is configured with: grub\nB is configured with: uefi", "", true},
		{"unknown", "System currently booted with uefi", "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := detectProxmoxBootMode(test.output)
			if (err != nil) != test.bad || got != test.want {
				t.Fatalf("detect=%q err=%v; want=%q bad=%v", got, err, test.want, test.bad)
			}
		})
	}
}

func TestSafetySnapshotErrorRequiresMachineIdentity(t *testing.T) {
	if err := safetySnapshotError(domain.Snapshot{}); err == nil || !strings.Contains(err.Error(), "machine identity") {
		t.Fatalf("missing machine identity was not blocked: %v", err)
	}
}

func TestMergeDriveTelemetryPreservesOnlyMatchingIdentity(t *testing.T) {
	fresh := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	temperature := 31.0
	previous := domain.Snapshot{Drives: []domain.Drive{
		{ID: "disk-a", WWN: "wwn-a", Serial: "serial-a", CapacityBytes: 1000, Health: domain.DriveHealth{Severity: domain.SeverityOK, TemperatureC: &temperature, FreshAt: fresh}},
		{ID: "disk-b", WWN: "wwn-b", Serial: "old-serial", CapacityBytes: 2000, Health: domain.DriveHealth{Severity: domain.SeverityOK, FreshAt: fresh}},
	}}
	current := domain.Snapshot{Drives: []domain.Drive{
		{ID: "disk-a", WWN: "wwn-a", Serial: "serial-a", CapacityBytes: 1000, Health: domain.DriveHealth{Severity: domain.SeverityUnknown}},
		{ID: "disk-b", WWN: "wwn-b", Serial: "new-serial", CapacityBytes: 2000, Health: domain.DriveHealth{Severity: domain.SeverityUnknown}},
	}}

	mergeDriveTelemetry(&current, previous, true)
	if current.Drives[0].Health.FreshAt != fresh || current.Drives[0].Health.TemperatureC == nil {
		t.Fatalf("matching telemetry was not preserved: %+v", current.Drives[0].Health)
	}
	if !current.Drives[1].Health.FreshAt.IsZero() {
		t.Fatalf("telemetry crossed an identity change: %+v", current.Drives[1].Health)
	}
}

func TestMergeDriveTelemetryKeepsLastValuesForSleepingDrive(t *testing.T) {
	fresh := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	wear := 8.0
	previous := domain.Snapshot{Drives: []domain.Drive{{
		ID: "disk-a", WWN: "wwn-a", Serial: "serial-a", CapacityBytes: 1000,
		Health: domain.DriveHealth{Severity: domain.SeverityOK, PercentageUsed: &wear, FreshAt: fresh},
	}}}
	current := domain.Snapshot{Drives: []domain.Drive{{
		ID: "disk-a", WWN: "wwn-a", Serial: "serial-a", CapacityBytes: 1000,
		Health: domain.DriveHealth{Severity: domain.SeverityUnknown, Sleeping: true, Message: "standby"},
	}}}

	mergeDriveTelemetry(&current, previous, false)
	health := current.Drives[0].Health
	if !health.Sleeping || health.FreshAt != fresh || health.PercentageUsed == nil || !strings.Contains(health.Message, "last collected") {
		t.Fatalf("sleeping telemetry merge=%+v", health)
	}
}

func TestMergeHardwareErrorsRetainsOnlyPresentDriveErrors(t *testing.T) {
	previous := domain.Snapshot{Drives: []domain.Drive{{ID: "disk-a"}, {ID: "changed", Serial: "old"}}, Errors: []domain.SourceError{
		{Source: "smart:disk-a", Kind: "permission", Message: "denied", Partial: true},
		{Source: "smart:changed", Kind: "timeout", Message: "old identity", Partial: true},
		{Source: "smart:removed", Kind: "timeout", Message: "old", Partial: true},
		{Source: "zpool.status", Kind: "parse", Message: "not hardware", Partial: true},
	}}
	current := domain.Snapshot{Drives: []domain.Drive{{ID: "disk-a"}, {ID: "changed", Serial: "new"}}}

	mergeHardwareErrors(&current, previous)
	if len(current.Errors) != 1 || current.Errors[0].Source != "smart:disk-a" || !current.Partial {
		t.Fatalf("merged errors=%+v partial=%t", current.Errors, current.Partial)
	}
}

func TestAppendTelemetryFreshnessFindings(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	snapshot := domain.Snapshot{Drives: []domain.Drive{
		{ID: "never"},
		{ID: "stale", Health: domain.DriveHealth{FreshAt: now.Add(-3 * time.Hour)}},
		{ID: "fresh", Health: domain.DriveHealth{FreshAt: now.Add(-time.Minute)}},
		{ID: "small-skew", Health: domain.DriveHealth{FreshAt: now.Add(30 * time.Second)}},
		{ID: "future", Health: domain.DriveHealth{FreshAt: now.Add(2 * time.Hour)}},
	}}

	appendTelemetryFreshnessFindings(&snapshot, now, 2*time.Hour)
	appendTelemetryFreshnessFindings(&snapshot, now, 2*time.Hour)
	if len(snapshot.Findings) != 3 {
		t.Fatalf("freshness findings=%+v", snapshot.Findings)
	}
	if snapshot.Findings[0].Severity != domain.SeverityUnknown || snapshot.Findings[1].Severity != domain.SeverityUnknown {
		t.Fatalf("freshness findings must be unknown: %+v", snapshot.Findings)
	}
	foundFuture := false
	for _, finding := range snapshot.Findings {
		foundFuture = foundFuture || finding.ID == "drive:future:smart-future"
	}
	if !foundFuture {
		t.Fatalf("future SMART time did not produce an unknown finding: %+v", snapshot.Findings)
	}
	if monitorCode(snapshot) != 3 {
		t.Fatalf("unknown drive telemetry did not produce monitoring exit 3")
	}
}
