// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/lockfile"
	"github.com/bouldinnathan/zpoolpilot/internal/monitor"
)

func TestInvalidMonitorOptionsPreserveCache(t *testing.T) {
	for _, args := range [][]string{
		{"collect", "--profile", "typo"},
		{"collect", "--profile", "fast", "--wake"},
		{"serve", "--max-age", "0s"},
		{"serve", "--max-age", "-1s"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			a := cancellationCommandTestApp(t)
			const original = "existing snapshot must survive invalid arguments"
			if err := os.WriteFile(a.cfg.SnapshotPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			code, err := a.runMonitor(context.Background(), args)
			if code != 64 || err == nil {
				t.Fatalf("code=%d error=%v; want usage failure", code, err)
			}
			got, err := os.ReadFile(a.cfg.SnapshotPath)
			if err != nil || string(got) != original {
				t.Fatalf("invalid options changed cache: %q, %v", got, err)
			}
			if _, err := os.Stat(a.cfg.CollectionLockPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid options reached collection lock: %v", err)
			}
		})
	}
}

func TestCachedStatusCommandTraceRequiresOptIn(t *testing.T) {
	for _, test := range []struct {
		name             string
		args             []string
		configured, want bool
	}{
		{name: "default"},
		{name: "requested", args: []string{"--include-commands"}, want: true},
		{name: "configured", configured: true, want: true},
		{name: "explicitly disabled", configured: true, args: []string{"--include-commands=false"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := cancellationCommandTestApp(t)
			a.cfg.IncludeCommands = test.configured
			snapshot := domain.Snapshot{Generated: time.Now().UTC(), Commands: []domain.CommandTrace{
				{Program: "zpool", Args: []string{"status"}, Capability: "observe", Risk: domain.RiskRead},
			}}
			if err := monitor.SaveAtomic(a.cfg.SnapshotPath, snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := a.runStatus(context.Background(), append([]string{"--json"}, test.args...)); err != nil {
				t.Fatal(err)
			}
			encoded, err := os.ReadFile(a.out.Name())
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["command_trace"]; present != test.want {
				t.Fatalf("command_trace present=%t, want %t: %s", present, test.want, encoded)
			}
			cached, err := monitor.Load(a.cfg.SnapshotPath)
			if err != nil || len(cached.Commands) != 1 {
				t.Fatalf("status changed cached trace: %+v, %v", cached.Commands, err)
			}
		})
	}
}

func TestLoadSnapshotReevaluatesCachedHardwareAge(t *testing.T) {
	a := cancellationCommandTestApp(t)
	snapshot := domain.Snapshot{Generated: time.Now().UTC(), Drives: []domain.Drive{{
		ID: "disk-a", Health: domain.DriveHealth{Severity: domain.SeverityOK, FreshAt: time.Now().Add(-3 * a.cfg.HardwareInterval)},
	}}}
	if err := monitor.SaveAtomic(a.cfg.SnapshotPath, snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range got.Findings {
		if finding.ID == "drive:disk-a:smart-stale" && finding.Severity == domain.SeverityUnknown {
			return
		}
	}
	t.Fatalf("stale cached health lacks an unknown finding: %+v", got.Findings)
}

func TestPersistedCollectorsLockBeforeDiscovery(t *testing.T) {
	a := cancellationCommandTestApp(t)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer close(release)
	go func() {
		_, collectErr, saveErr := a.collectAndSaveSnapshot(ctx, false, true, func(ctx context.Context) (domain.Snapshot, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return domain.Snapshot{}, ctx.Err()
			}
			return domain.Snapshot{APIVersion: domain.SnapshotAPIVersion, Generated: time.Now().UTC()}, nil
		})
		done <- errors.Join(collectErr, saveErr)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first collector did not start")
	}
	called := false
	_, collectErr, saveErr := a.collectAndSaveSnapshot(ctx, true, false, func(context.Context) (domain.Snapshot, error) {
		called = true
		return domain.Snapshot{}, nil
	})
	if called || collectErr != nil || !errors.Is(saveErr, lockfile.ErrBusy) {
		t.Errorf("overlapping collector reached discovery: called=%t collect=%v save=%v", called, collectErr, saveErr)
	}
	// Cancellation releases the first callback without publishing its incomplete
	// result, and must release the lock for the next collector.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first collector lost cancellation: %v", err)
	}
	lock, err := lockfile.Acquire(context.Background(), a.cfg.CollectionLockPath, false)
	if err != nil {
		t.Fatalf("collector retained its lock after cancellation: %v", err)
	}
	_ = lock.Close()
}

func TestIncompleteCollectionPreservesPreviousSnapshot(t *testing.T) {
	a := cancellationCommandTestApp(t)
	const original = "previous cache"
	if err := os.WriteFile(a.cfg.SnapshotPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("collector failed after partial discovery")
	_, collectErr, saveErr := a.collectAndSaveSnapshot(context.Background(), false, false, func(context.Context) (domain.Snapshot, error) {
		return domain.Snapshot{APIVersion: domain.SnapshotAPIVersion, Generated: time.Now().UTC(), Partial: true}, wantErr
	})
	if !errors.Is(collectErr, wantErr) || saveErr != nil {
		t.Fatalf("collect=%v save=%v", collectErr, saveErr)
	}
	got, err := os.ReadFile(a.cfg.SnapshotPath)
	if err != nil || string(got) != original {
		t.Fatalf("incomplete collection replaced previous snapshot: %q, %v", got, err)
	}
}

func TestResultSnapshotCannotReplaceNewerCollectorCache(t *testing.T) {
	a := cancellationCommandTestApp(t)
	now := time.Now().UTC()
	previous := domain.Snapshot{APIVersion: domain.SnapshotAPIVersion, Generated: now, Host: domain.Host{Hostname: "newer"}}
	if err := monitor.SaveAtomic(a.cfg.SnapshotPath, previous); err != nil {
		t.Fatal(err)
	}
	older := domain.Snapshot{APIVersion: domain.SnapshotAPIVersion, Generated: now.Add(-time.Minute)}
	if err := a.saveSnapshot(context.Background(), &older, false, true); err == nil {
		t.Fatal("older apply result overwrote newer collector cache")
	}
	got, err := monitor.Load(a.cfg.SnapshotPath)
	if err != nil || got.Host.Hostname != "newer" || !got.Generated.Equal(now) {
		t.Fatalf("newer cache changed: %+v, %v", got, err)
	}
}
