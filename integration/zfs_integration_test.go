// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build zfsintegration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

const destructiveAcknowledgement = "DESTROY_TEMPORARY_ZFS_TEST_POOLS"

var integrationPoolPattern = regexp.MustCompile(`^zpoolpilot_ci_[0-9]+_[0-9]+_[0-9]+$`)

func TestLiveTemporaryPoolCollection(t *testing.T) {
	requireIntegrationGuard(t)

	binary := requiredEnv(t, "ZPOOLPILOT_INTEGRATION_BINARY")
	config := requiredEnv(t, "ZPOOLPILOT_INTEGRATION_CONFIG")
	poolName := requiredEnv(t, "ZPOOLPILOT_INTEGRATION_POOL")
	poolGUID := requiredEnv(t, "ZPOOLPILOT_INTEGRATION_POOL_GUID")
	if !integrationPoolPattern.MatchString(poolName) {
		t.Fatalf("unsafe integration pool name %q", poolName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := runCLI(ctx, binary, "--config", config, "--read-only", "monitor", "collect", "--profile", "fast", "--include-commands", "--wait")
	if result.exitCode < 0 || result.exitCode > 3 {
		t.Fatalf("monitor collect exit=%d\nstdout:\n%s\nstderr:\n%s", result.exitCode, result.stdout, result.stderr)
	}

	result = runCLI(ctx, binary, "--config", config, "--read-only", "status", "--json", "--include-commands")
	if result.exitCode < 0 || result.exitCode > 3 {
		t.Fatalf("status exit=%d\nstdout:\n%s\nstderr:\n%s", result.exitCode, result.stdout, result.stderr)
	}
	var snapshot domain.Snapshot
	if err := json.Unmarshal(result.stdout, &snapshot); err != nil {
		t.Fatalf("decode status JSON: %v\nstdout:\n%s\nstderr:\n%s", err, result.stdout, result.stderr)
	}
	if snapshot.APIVersion != domain.SnapshotAPIVersion {
		t.Fatalf("api_version=%q, want %q", snapshot.APIVersion, domain.SnapshotAPIVersion)
	}

	var pool *domain.Pool
	for index := range snapshot.Pools {
		if snapshot.Pools[index].Name == poolName {
			pool = &snapshot.Pools[index]
			break
		}
	}
	if pool == nil {
		t.Fatalf("temporary pool %q absent from snapshot: %#v", poolName, snapshot.Pools)
	}
	if pool.GUID != poolGUID {
		t.Fatalf("pool GUID=%q, want %q", pool.GUID, poolGUID)
	}
	if pool.State != "ONLINE" {
		t.Fatalf("pool state=%q, want ONLINE", pool.State)
	}
	if !hasVDevType(pool.VDevs, "mirror") {
		t.Fatalf("temporary pool has no mirror vdev: %#v", pool.VDevs)
	}
	if !hasVDevClass(pool.VDevs, "spare") {
		t.Fatalf("temporary pool has no spare class: %#v", pool.VDevs)
	}
	if len(snapshot.Commands) == 0 {
		t.Fatal("snapshot did not retain requested command traces")
	}
	for _, trace := range snapshot.Commands {
		if trace.Capability != "observe" || trace.Risk != domain.RiskRead {
			t.Fatalf("non-observer command escaped collection boundary: %#v", trace)
		}
	}
}

func TestReadOnlyModeRejectsApplyBeforeReadingPlan(t *testing.T) {
	requireIntegrationGuard(t)
	binary := requiredEnv(t, "ZPOOLPILOT_INTEGRATION_BINARY")
	config := requiredEnv(t, "ZPOOLPILOT_INTEGRATION_CONFIG")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := runCLI(ctx, binary, "--config", config, "--read-only", "apply", "/does/not/exist.plan.json")
	if result.exitCode != 77 {
		t.Fatalf("read-only apply exit=%d, want 77\nstdout:\n%s\nstderr:\n%s", result.exitCode, result.stdout, result.stderr)
	}
}

func requireIntegrationGuard(t *testing.T) {
	t.Helper()
	if os.Getenv("ZPOOLPILOT_DESTRUCTIVE_TESTS") != destructiveAcknowledgement {
		t.Skip("destructive integration acknowledgement is absent")
	}
	if os.Getenv("ZPOOLPILOT_INTEGRATION_VM") != "ephemeral" {
		t.Fatal("ZPOOLPILOT_INTEGRATION_VM must attest an ephemeral VM")
	}
	if os.Geteuid() != 0 {
		t.Fatal("live ZFS integration tests require root")
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

type commandResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

func runCLI(ctx context.Context, binary string, args ...string) commandResult {
	command := exec.CommandContext(ctx, binary, args...)
	// Bound pipe draining if a timed-out CLI leaves a discovery child holding
	// stdout/stderr open. The suite timeout must not depend on child cleanup.
	command.WaitDelay = 5 * time.Second
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = -1
			_, _ = fmt.Fprintln(&stderr, err)
		}
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}
}

func hasVDevType(vdevs []domain.VDev, expected string) bool {
	for _, vdev := range vdevs {
		if vdev.Type == expected || hasVDevType(vdev.Children, expected) {
			return true
		}
	}
	return false
}

func hasVDevClass(vdevs []domain.VDev, expected string) bool {
	for _, vdev := range vdevs {
		if vdev.Class == expected || hasVDevClass(vdev.Children, expected) {
			return true
		}
	}
	return false
}
