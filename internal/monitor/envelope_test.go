// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func validEnvelopeSnapshot() domain.Snapshot {
	return domain.Snapshot{
		APIVersion: domain.SnapshotAPIVersion,
		Generated:  time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		Host:       domain.Host{Hostname: "node-a"},
	}
}

func TestValidateSnapshotRejectsExcessiveEntityCounts(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*domain.Snapshot)
		text   string
	}{
		{"pools", func(snapshot *domain.Snapshot) { snapshot.Pools = make([]domain.Pool, maxSnapshotPools+1) }, "pools count"},
		{"datasets", func(snapshot *domain.Snapshot) { snapshot.Datasets = make([]domain.Dataset, maxSnapshotDatasets+1) }, "datasets count"},
		{"drives", func(snapshot *domain.Snapshot) { snapshot.Drives = make([]domain.Drive, maxSnapshotDrives+1) }, "drives count"},
		{"findings", func(snapshot *domain.Snapshot) { snapshot.Findings = make([]domain.Finding, maxSnapshotFindings+1) }, "findings count"},
		{"errors", func(snapshot *domain.Snapshot) { snapshot.Errors = make([]domain.SourceError, maxSnapshotErrors+1) }, "source_errors count"},
		{"commands", func(snapshot *domain.Snapshot) {
			snapshot.Commands = make([]domain.CommandTrace, maxSnapshotCommands+1)
		}, "command_trace count"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validEnvelopeSnapshot()
			test.mutate(&snapshot)
			if err := ValidateSnapshot(snapshot, now); err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("ValidateSnapshot() error = %v, want %q", err, test.text)
			}
		})
	}
}

func TestValidateSnapshotRejectsDeepOrExcessiveVDevTrees(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	snapshot := validEnvelopeSnapshot()
	root := domain.VDev{Name: "leaf"}
	for depth := 0; depth < maxSnapshotVDevDepth; depth++ {
		root = domain.VDev{Name: fmt.Sprintf("level-%d", depth), Children: []domain.VDev{root}}
	}
	snapshot.Pools = []domain.Pool{{Name: "tank", VDevs: []domain.VDev{root}}}
	if err := ValidateSnapshot(snapshot, now); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("deep vdev tree accepted: %v", err)
	}

	snapshot = validEnvelopeSnapshot()
	snapshot.Pools = []domain.Pool{{Name: "tank", VDevs: make([]domain.VDev, maxSnapshotVDevs+1)}}
	if err := ValidateSnapshot(snapshot, now); err == nil || !strings.Contains(err.Error(), "vdev count") {
		t.Fatalf("excessive vdev tree accepted: %v", err)
	}
}

func TestValidateSnapshotRejectsOversizedStringsMapsAndArgv(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	snapshot := validEnvelopeSnapshot()
	snapshot.Host.Hostname = strings.Repeat("x", maxSnapshotStringBytes+1)
	if err := ValidateSnapshot(snapshot, now); err == nil || !strings.Contains(err.Error(), "host.hostname") {
		t.Fatalf("oversized string accepted: %v", err)
	}

	snapshot = validEnvelopeSnapshot()
	properties := make(map[string]string, maxSnapshotMapEntries+1)
	for index := 0; index <= maxSnapshotMapEntries; index++ {
		properties[fmt.Sprintf("property-%d", index)] = "on"
	}
	snapshot.Datasets = []domain.Dataset{{Name: "tank/data", Properties: properties}}
	if err := ValidateSnapshot(snapshot, now); err == nil || !strings.Contains(err.Error(), "properties has") {
		t.Fatalf("oversized property map accepted: %v", err)
	}

	snapshot = validEnvelopeSnapshot()
	snapshot.Commands = []domain.CommandTrace{{Program: "zpool", Args: make([]string, maxSnapshotCommandArgs+1)}}
	if err := ValidateSnapshot(snapshot, now); err == nil || !strings.Contains(err.Error(), "args count") {
		t.Fatalf("oversized command argv accepted: %v", err)
	}
}

func TestValidateSnapshotAcceptsOperationalEnvelope(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	snapshot := validEnvelopeSnapshot()
	snapshot.Pools = []domain.Pool{{
		Name: "tank", GUID: "123", State: "ONLINE",
		VDevs: []domain.VDev{{Name: "mirror-0", Children: []domain.VDev{{Name: "disk-a"}, {Name: "disk-b"}}}},
	}}
	snapshot.Datasets = []domain.Dataset{{
		Name: "tank/data", Type: "filesystem",
		Properties: map[string]string{"compression": "lz4"}, PropertySources: map[string]string{"compression": "local"},
	}}
	snapshot.Drives = []domain.Drive{{ID: "wwn:drive-a", Serial: "drive-a"}}
	snapshot.Findings = []domain.Finding{{ID: "example", Severity: domain.SeverityInfo, Scope: "host", Title: "example"}}
	if err := ValidateSnapshot(snapshot, now); err != nil {
		t.Fatalf("normal snapshot rejected: %v", err)
	}
}
