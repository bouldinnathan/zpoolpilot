// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestDefaultSMARTStopsAtUnknownPowerOrTransport(t *testing.T) {
	for _, transport := range []string{"sata", "ata", "sas", "scsi", "usb", ""} {
		t.Run(transport, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]Result{
				commandKey("smartctl", "--json=c", "--all", "--device", "sat", "--nocheck", "standby,2,2", "/dev/sda"): {
					ExitCode: 2, Stdout: []byte(`{"smartctl":{"exit_status":2,"messages":[{"string":"CHECK POWER MODE not supported, exit(2)"}]}}`),
				},
			}}
			snapshot := domain.Snapshot{Generated: time.Now()}
			state := collectionState{collector: New(runner), options: (Options{}).withDefaults(), snapshot: &snapshot}
			drives := []domain.Drive{{ID: "test", DevNode: "/dev/sda", Transport: transport}}
			state.collectSMART(context.Background(), drives)
			state.finish()
			health := drives[0].Health
			if !snapshot.Partial || len(snapshot.Errors) != 1 || health.Severity != domain.SeverityUnknown || !health.FreshAt.IsZero() || health.SMARTPassed != nil || health.Sleeping {
				t.Fatalf("unsupported check became usable telemetry: health=%+v errors=%+v", health, snapshot.Errors)
			}
			if transport == "sata" || transport == "ata" {
				if len(runner.calls) != 1 {
					t.Fatalf("expected one guarded read and no retry: %+v", runner.calls)
				}
			} else if len(runner.calls) != 0 || !health.Unsupported {
				t.Fatalf("unguarded transport was probed: calls=%+v health=%+v", runner.calls, health)
			}
		})
	}
}

func TestGuardedSMARTPreservesSleepingAndActiveSamples(t *testing.T) {
	for _, test := range []struct {
		name, transport, device, fixture string
		exitCode                         int
		sleeping                         bool
	}{
		{"sleeping SATA", "sata", "sat", "smart_sleeping.json", 2, true},
		{"active SATA", "ata", "sat", "smart_ata.json", 0, false},
		{"legacy ATA", "ata", "ata", "smart_ata.json", 0, false},
		{"NVMe", "nvme", "nvme", "smart_nvme.json", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/dev/sda"
			if test.device == "ata" {
				path = "/dev/hda"
			}
			if test.device == "nvme" {
				path = "/dev/nvme0n1"
			}
			args := []string{"--json=c", "--all", "--device", test.device}
			if test.device != "nvme" {
				args = append(args, "--nocheck", "standby,2,2")
			}
			args = append(args, path)
			runner := &fakeRunner{responses: map[string]Result{commandKey("smartctl", args...): {ExitCode: test.exitCode, Stdout: fixture(t, test.fixture)}}}
			snapshot := domain.Snapshot{Generated: time.Now()}
			state := collectionState{collector: New(runner), options: (Options{}).withDefaults(), snapshot: &snapshot}
			drives := []domain.Drive{{ID: "test", DevNode: path, Transport: test.transport}}
			state.collectSMART(context.Background(), drives)
			health := drives[0].Health
			if len(runner.calls) != 1 || len(snapshot.Errors) != 0 || health.Sleeping != test.sleeping || health.FreshAt.IsZero() != test.sleeping {
				t.Fatalf("guarded sample mishandled: health=%+v calls=%+v errors=%+v", health, runner.calls, snapshot.Errors)
			}
		})
	}
}

func TestLegacySleepingSMARTShapeStillSupported(t *testing.T) {
	health, err := parseSMART([]byte(`{"power_mode":"STANDBY"}`), 2, time.Now())
	if err != nil || !health.Sleeping || !health.FreshAt.IsZero() {
		t.Fatalf("legacy sleeping sample: %+v, %v", health, err)
	}
}
