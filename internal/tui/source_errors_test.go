// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestPoolCapacityFailureDoesNotRenderHealthyZeros(t *testing.T) {
	snapshot := domain.Snapshot{Pools: []domain.Pool{{Name: "tank", State: "ONLINE"}}, Errors: []domain.SourceError{{Source: "zpool.list", Partial: true}}}
	for name, render := range map[string]func(io.Writer, domain.Snapshot, bool){"summary": Summary, "overview": overview, "pools": pools} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			render(&output, snapshot, false)
			got := output.String()
			if !strings.Contains(got, "UNKNOWN") || strings.Contains(got, "0.0%") || strings.Contains(got, "0 B") {
				t.Fatalf("failed capacity source presented measurements: %s", got)
			}
		})
	}
}

func TestStatusFailureDoesNotReportNoTasksOrHealthyErrors(t *testing.T) {
	snapshot := richSnapshot()
	snapshot.Errors = []domain.SourceError{{Source: "zpool.status", Partial: true}, {Source: "arcstats", Partial: true}}
	for name, render := range map[string]func(io.Writer, domain.Snapshot, bool){"summary": Summary, "pools": pools, "tasks": tasks, "arc": arc} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			render(&output, snapshot, false)
			got := output.String()
			if !strings.Contains(got, "UNKNOWN") || strings.Contains(got, "No current or recent") || strings.Contains(got, "errors read=") || strings.Contains(got, "hit ratio 80.00%") {
				t.Fatalf("failed source displayed a definitive measurement: %s", got)
			}
		})
	}
	var output bytes.Buffer
	Summary(&output, domain.Snapshot{Errors: snapshot.Errors}, false)
	if !strings.Contains(output.String(), "Imported pools are UNKNOWN") {
		t.Fatalf("failed discovery was shown as an empty host: %s", output.String())
	}
}

func TestSuccessfulTextFallbackKeepsDashboardMeasurements(t *testing.T) {
	snapshot := domain.Snapshot{Pools: []domain.Pool{{Name: "tank", State: "ONLINE", CapacityPercent: 42}}, Errors: []domain.SourceError{{Source: "zpool.list.json", Partial: true}}}
	var output bytes.Buffer
	Summary(&output, snapshot, false)
	if !strings.Contains(output.String(), "42.0%") {
		t.Fatalf("successful text fallback measurement was hidden: %s", output.String())
	}
}

func TestDashboardRedrawReevaluatesHealthAge(t *testing.T) {
	snapshot := domain.Snapshot{Drives: []domain.Drive{{ID: "disk-a", Health: domain.DriveHealth{
		Severity: domain.SeverityOK, FreshAt: time.Now().Add(-time.Hour),
	}}}}
	frame := renderFrame(snapshot, 2, "ok", false, Options{HardwareMaxAge: time.Minute}, 160, 30)
	if strings.Contains(frame, "[GREEN]") || !strings.Contains(frame, "[UNKNOWN]") {
		t.Fatalf("stale dashboard drive still looks healthy: %s", frame)
	}
	if len(snapshot.Findings) != 0 || snapshot.Drives[0].Health.Severity != domain.SeverityOK {
		t.Fatal("rendering changed the original cached health assessment")
	}
}
