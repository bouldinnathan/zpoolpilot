// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestRefreshTelemetryFindingsReplacesStaleFindingsAndRetainsHealthFailures(t *testing.T) {
	now := time.Now().UTC()
	snapshot := domain.Snapshot{
		Drives: []domain.Drive{{ID: "disk-a", Health: domain.DriveHealth{FreshAt: now.Add(-time.Hour)}}},
		Findings: []domain.Finding{
			{ID: "drive:disk-a:smart-never-collected", Severity: domain.SeverityUnknown},
			{ID: "drive:disk-a:smart-future", Severity: domain.SeverityUnknown},
			{ID: "drive:removed:smart-stale", Severity: domain.SeverityUnknown},
			{ID: "drive:disk-a:failed", Severity: domain.SeverityCritical},
		},
	}
	RefreshTelemetryFindings(&snapshot, now, 30*time.Minute)
	RefreshTelemetryFindings(&snapshot, now.Add(time.Hour), 30*time.Minute)
	if len(snapshot.Findings) != 2 || snapshot.Findings[0].ID != "drive:disk-a:failed" ||
		snapshot.Findings[1].ID != "drive:disk-a:smart-stale" || !strings.Contains(snapshot.Findings[1].Evidence, "2h0m0s") {
		t.Fatalf("freshness findings were not recomputed: %+v", snapshot.Findings)
	}
	snapshot.Drives[0].Health.FreshAt = now
	RefreshTelemetryFindings(&snapshot, now, 30*time.Minute)
	if len(snapshot.Findings) != 1 || snapshot.Findings[0].Severity != domain.SeverityCritical {
		t.Fatalf("new telemetry did not remove stale finding while retaining health failure: %+v", snapshot.Findings)
	}
}

func TestDriveSeverityKeepsHardwareFailuresAndUsesCurrentFindings(t *testing.T) {
	drive := domain.Drive{ID: "disk-a", Health: domain.DriveHealth{Severity: domain.SeverityOK}}
	snapshot := domain.Snapshot{Findings: []domain.Finding{
		{ID: "drive:disk-a:smart-stale", Scope: "drive/disk-a", Severity: domain.SeverityUnknown},
		{ID: "drive:disk-b:failed", Scope: "drive/disk-b", Severity: domain.SeverityCritical},
	}}
	if got := DriveSeverity(snapshot, drive); got != domain.SeverityUnknown {
		t.Fatalf("stale drive severity=%q, want unknown", got)
	}
	if drive.Health.Severity != domain.SeverityOK {
		t.Fatal("severity projection changed historical sample")
	}
	drive.Health.Severity = domain.SeverityCritical
	if got := DriveSeverity(snapshot, drive); got != domain.SeverityCritical {
		t.Fatalf("staleness hid hardware failure: %q", got)
	}
	if got := DriveSeverity(domain.Snapshot{}, domain.Drive{}); got != domain.SeverityUnknown {
		t.Fatalf("missing hardware severity=%q, want unknown", got)
	}
}
