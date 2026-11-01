// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package execx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestDefaultAllowlistSeparatesCapabilities(t *testing.T) {
	t.Parallel()
	allowlist := DefaultAllowlist()
	tests := []struct {
		name    string
		command Command
		wantErr error
	}{
		{"status is observable", Command{Program: "zpool", Args: []string{"status", "-j", "--json-int", "-P", "-t"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, nil},
		{"unemitted status shape is rejected", Command{Program: "zpool", Args: []string{"status", "-j"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"clearing zpool events is not observable", Command{Program: "zpool", Args: []string{"events", "-c"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"create is not observable", Command{Program: "zpool", Args: []string{"create", "tank", "/dev/disk/by-id/wwn-a"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"create is mutable", Command{Program: "zpool", Args: []string{"create", "tank", "/dev/disk/by-id/wwn-a"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, nil},
		{"scrub start is yellow mutation", Command{Program: "zpool", Args: []string{"scrub", "tank"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, nil},
		{"scrub stop is yellow mutation", Command{Program: "zpool", Args: []string{"scrub", "-s", "tank"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, nil},
		{"trim option is forbidden", Command{Program: "zpool", Args: []string{"trim", "-r", "1G", "tank"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"clear rewind is forbidden", Command{Program: "zpool", Args: []string{"clear", "-F", "tank"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"force is forbidden", Command{Program: "zpool", Args: []string{"create", "-f", "tank", "/dev/disk/by-id/wwn-a"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"volatile disk path is forbidden", Command{Program: "zpool", Args: []string{"replace", "tank", "123", "/dev/sda"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"smart health is observable", Command{Program: "smartctl", Args: []string{"--json=c", "--all", "--device", "sat", "--nocheck", "standby,2,2", "/dev/sda"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, nil},
		{"unsupported-power fallback rejected", Command{Program: "smartctl", Args: []string{"--json=c", "--all", "--nocheck", "standby", "/dev/sda"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"SCSI cannot claim fail-closed standby", Command{Program: "smartctl", Args: []string{"--json=c", "--all", "--device", "scsi", "--nocheck", "standby,2,2", "/dev/sda"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"waking smart health is observable", Command{Program: "smartctl", Args: []string{"--json=c", "--all", "/dev/sda"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, nil},
		{"smart test is not observable", Command{Program: "smartctl", Args: []string{"-t", "long", "/dev/sda"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"smart mutation is outside implemented workflows", Command{Program: "smartctl", Args: []string{"-t", "long", "/dev/sda"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"unused hostname query is not observable", Command{Program: "hostname", Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"hostname change is not observable", Command{Program: "hostname", Args: []string{"new-hostname"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"zero argument pveversion is observable", Command{Program: "pveversion", Capability: CapabilityObserve, Risk: domain.RiskRead}, nil},
		{"pveversion arguments are rejected", Command{Program: "pveversion", Args: []string{"--verbose"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"SES control is not observable", Command{Program: "sg_ses", Args: []string{"--control", "--set=ident", "/dev/sg0"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrNotAllowed},
		{"specific SES locate is mutable", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=12", "--set=ident", "/dev/sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, nil},
		{"slot zero SES locate is mutable", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=0", "--clear=ident", "/dev/sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, nil},
		{"SES element index is forbidden", Command{Program: "sg_ses", Args: []string{"--index=12", "--set=ident", "/dev/sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"negative SES slot is forbidden", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=-1", "--set=ident", "/dev/sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"non-SES device is forbidden", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=1", "--set=ident", "/dev/sda"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"nested by-id mutation is forbidden", Command{Program: "wipefs", Args: []string{"--all", "/dev/disk/by-id/nested/device"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"unknown risk is forbidden", Command{Program: "wipefs", Args: []string{"--all", "/dev/disk/by-id/wwn-a"}, Capability: CapabilityMutate, Risk: domain.Risk("pink")}, ErrInvalidCommand},
		{"SES traversal path is forbidden", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=12", "--set=ident", "/dev/../tmp/sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"SES non-clean path is forbidden", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=12", "--clear=ident", "/dev//sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"arbitrary SES change is forbidden", Command{Program: "sg_ses", Args: []string{"--dev-slot-num=12", "--set=fault", "/dev/sg0"}, Capability: CapabilityMutate, Risk: domain.RiskChange}, ErrNotAllowed},
		{"boot table clone is mutable", Command{Program: "sgdisk", Args: []string{"/dev/disk/by-id/wwn-source", "-R", "/dev/disk/by-id/wwn-target"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, nil},
		{"boot table clone volatile source rejected", Command{Program: "sgdisk", Args: []string{"/dev/sda", "-R", "/dev/disk/by-id/wwn-target"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"boot table alternate replicate syntax rejected", Command{Program: "sgdisk", Args: []string{"--replicate=/dev/disk/by-id/wwn-target", "/dev/disk/by-id/wwn-source"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"boot GUID randomization is mutable", Command{Program: "sgdisk", Args: []string{"-G", "/dev/disk/by-id/wwn-target"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, nil},
		{"boot ESP format is mutable", Command{Program: "proxmox-boot-tool", Args: []string{"format", "/dev/disk/by-id/wwn-target-part2"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, nil},
		{"boot GRUB init is mutable", Command{Program: "proxmox-boot-tool", Args: []string{"init", "/dev/disk/by-id/wwn-target-part2", "grub"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, nil},
		{"wrong ESP partition rejected", Command{Program: "proxmox-boot-tool", Args: []string{"format", "/dev/disk/by-id/wwn-target-part3"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"plain grub executable absent", Command{Program: "grub-install", Args: []string{"/dev/disk/by-id/wwn-target"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"shell is absent", Command{Program: "sh", Args: []string{"-c", "zpool destroy tank"}, Capability: CapabilityMutate, Risk: domain.RiskDestructive}, ErrNotAllowed},
		{"executable path rejected", Command{Program: "/bin/echo", Args: []string{"ok"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrInvalidCommand},
		{"NUL rejected", Command{Program: "zpool", Args: []string{"status", "bad\x00arg"}, Capability: CapabilityObserve, Risk: domain.RiskRead}, ErrInvalidCommand},
		{"observe risk mismatch", Command{Program: "zpool", Args: []string{"status"}, Capability: CapabilityObserve, Risk: domain.RiskChange}, ErrInvalidCommand},
		{"interactive observation rejected", Command{Program: "zpool", Args: []string{"status"}, Capability: CapabilityObserve, Risk: domain.RiskRead, Interactive: true}, ErrInvalidCommand},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := allowlist.Validate(test.command)
			if test.wantErr == nil && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestDefaultAllowlistAcceptsEveryEmittedObserverCommand(t *testing.T) {
	t.Parallel()
	const lsblkColumns = "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,VENDOR,REV,ROTA,TRAN,LOG-SEC,PHY-SEC,MOUNTPOINTS,FSTYPE,PKNAME"
	commands := []Command{
		{Program: "zpool", Args: []string{"version", "-j"}},
		{Program: "zpool", Args: []string{"--version"}},
		{Program: "zpool", Args: []string{"status", "-j", "--json-int", "-P", "-t"}},
		{Program: "zpool", Args: []string{"status", "-P", "-p", "-t"}},
		{Program: "zpool", Args: []string{"list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"}},
		{Program: "zpool", Args: []string{"list", "-H", "-p", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"}},
		{Program: "zpool", Args: []string{"get", "-j", "--json-int", "all"}},
		{Program: "zpool", Args: []string{"get", "-H", "-p", "-o", "name,property,value,source", "all"}},
		{Program: "zpool", Args: []string{"iostat", "-H", "-p"}},
		{Program: "zfs", Args: []string{"get", "-j", "--json-int", "all"}},
		{Program: "zfs", Args: []string{"get", "-H", "-p", "-o", "name,property,value,source", "all"}},
		{Program: "lsblk", Args: []string{"--json", "--bytes", "--paths", "--output", lsblkColumns}},
		{Program: "udevadm", Args: []string{"info", "--query=property", "--name", "/dev/sda"}},
		{Program: "smartctl", Args: []string{"--json=c", "--all", "/dev/sda"}},
		{Program: "smartctl", Args: []string{"--json=c", "--all", "--device", "sat", "--nocheck", "standby,2,2", "/dev/sda"}},
		{Program: "smartctl", Args: []string{"--json=c", "--all", "--device", "ata", "--nocheck", "standby,2,2", "/dev/hda"}},
		{Program: "smartctl", Args: []string{"--json=c", "--all", "--device", "nvme", "/dev/nvme0n1"}},
		{Program: "pveversion"},
		{Program: "proxmox-boot-tool", Args: []string{"status"}},
	}
	allowlist := DefaultAllowlist()
	for _, command := range commands {
		command.Capability = CapabilityObserve
		command.Risk = domain.RiskRead
		if err := allowlist.Validate(command); err != nil {
			t.Errorf("Validate(%s %q) = %v", command.Program, command.Args, err)
		}
	}
}

func TestOSRunnerIgnoresCallerPathAndEnvironment(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "env")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'HIJACKED=yes\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("ZPOOLPILOT_SHOULD_NOT_LEAK", "secret")
	resolved, err := ResolveProgram("env")
	if err != nil {
		t.Fatal(err)
	}
	if resolved == fake || !strings.HasPrefix(resolved, "/") {
		t.Fatalf("resolved untrusted executable %q", resolved)
	}
	runner := NewOSRunner(Config{Allowlist: Allowlist{Observe: []Rule{{Program: "env", Match: anyArgs}}}})
	result, err := runner.Run(context.Background(), Command{Program: "env", Capability: CapabilityObserve, Risk: domain.RiskRead})
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Stdout)
	if strings.Contains(output, "HIJACKED") || strings.Contains(output, "ZPOOLPILOT_SHOULD_NOT_LEAK") || !strings.Contains(output, "PATH="+TrustedPath) {
		t.Fatalf("subprocess environment was not isolated: %q", output)
	}
}

func TestInteractiveFakeHidesOutput(t *testing.T) {
	t.Parallel()
	runner := NewFakeRunner(false, FakeResponse{Stdout: []byte("secret output"), Stderr: []byte("prompt")})
	result, err := runner.Run(context.Background(), Command{
		Program: "zpool", Args: []string{"create", "-O", "encryption=on", "-O", "keylocation=prompt", "tank", "/dev/disk/by-id/wwn-a"},
		Capability: CapabilityMutate, Risk: domain.RiskDestructive, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stdout) != 0 || len(result.Stderr) != 0 || !result.Trace.OutputHidden {
		t.Fatalf("interactive output leaked into result: %#v", result)
	}
}

func TestFailureStderrDetailIsOneLineBoundedAndNeverTraced(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("exit status 1")
	stderr := strings.Repeat("x", 400) + "\nSECOND-LINE-SECRET"
	runner := NewFakeRunner(false, FakeResponse{Stderr: []byte(stderr), Err: sentinel})
	result, err := runner.Run(context.Background(), Command{
		Program: "zpool", Args: []string{"version", "-j"}, Capability: CapabilityObserve, Risk: domain.RiskRead,
	})
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "stderr: ") {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Contains(err.Error(), "SECOND-LINE-SECRET") || len(strings.TrimPrefix(err.Error(), sentinel.Error()+": stderr: ")) > maxStderrDetailBytes {
		t.Fatalf("stderr detail was not bounded to one line: %q", err)
	}
	if strings.Contains(result.Trace.Error, "stderr") || result.Trace.Error != sentinel.Error() {
		t.Fatalf("stderr leaked into trace: %#v", result.Trace)
	}
	if detail := boundedOneLine([]byte("bad\x1b[31m color\rsecond")); strings.ContainsRune(detail, '\x1b') || strings.Contains(detail, "second") {
		t.Fatalf("control or second-line content survived sanitization: %q", detail)
	}
}

func TestHiddenFailureNeverIncludesStderrDetail(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("exit status 1")
	runner := NewFakeRunner(false, FakeResponse{Stderr: []byte("PASSPHRASE-LEAK"), Err: sentinel})
	result, err := runner.Run(context.Background(), Command{
		Program: "zpool", Args: []string{"version", "-j"}, Capability: CapabilityObserve,
		Risk: domain.RiskRead, OutputHidden: true,
	})
	if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "PASSPHRASE-LEAK") {
		t.Fatalf("hidden stderr leaked: %v", err)
	}
	if len(result.Stdout) != 0 || len(result.Stderr) != 0 || !result.Trace.OutputHidden {
		t.Fatalf("hidden result exposed output: %#v", result)
	}
}

func TestReadOnlyFakeRejectsMutationBeforeDispatch(t *testing.T) {
	t.Parallel()
	runner := NewFakeRunner(true, FakeResponse{})
	_, err := runner.Run(context.Background(), Command{
		Program: "wipefs", Args: []string{"--all", "/dev/disk/by-id/wwn-a"},
		Capability: CapabilityMutate, Risk: domain.RiskDestructive,
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Run() error = %v, want ErrReadOnly", err)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("read-only runner dispatched %d calls", len(calls))
	}
}

func TestFakeRunnerCapturesDefensiveCopiesAndTimeout(t *testing.T) {
	t.Parallel()
	runner := NewFakeRunner(false,
		FakeResponse{Stdout: []byte("first")},
		FakeResponse{Delay: 100 * time.Millisecond},
	)
	args := []string{"version", "-j"}
	result, err := runner.Run(context.Background(), Command{Program: "zpool", Args: args, Capability: CapabilityObserve, Risk: domain.RiskRead})
	if err != nil || string(result.Stdout) != "first" {
		t.Fatalf("first Run() = %q, %v", result.Stdout, err)
	}
	args[0] = "destroy"
	if got := runner.Calls()[0].Args[0]; got != "version" {
		t.Fatalf("stored args changed to %q", got)
	}
	_, err = runner.Run(context.Background(), Command{Program: "zpool", Args: []string{"version", "-j"}, Capability: CapabilityObserve, Risk: domain.RiskRead, Timeout: time.Millisecond})
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("timed Run() error = %v, want ErrTimedOut", err)
	}
	traces := runner.Traces()
	if len(traces) != 2 || !traces[1].TimedOut || traces[1].ExitCode != -1 {
		t.Fatalf("unexpected traces: %#v", traces)
	}
}

func TestOSRunnerDirectArgvLocaleOutputAndTrace(t *testing.T) {
	t.Parallel()
	var callbackCount int
	runner := NewOSRunner(Config{
		Allowlist: Allowlist{Observe: []Rule{{Program: "env", Match: anyArgs}}},
		OnTrace:   func(domain.CommandTrace) { callbackCount++ },
	})
	result, err := runner.Run(context.Background(), Command{Program: "env", Capability: CapabilityObserve, Risk: domain.RiskRead})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	output := string(result.Stdout)
	if !strings.Contains(output, "LC_ALL=C\n") || !strings.Contains(output, "LANG=C\n") {
		t.Fatalf("locale not forced in output: %q", output)
	}
	if result.Trace.ExitCode != 0 || callbackCount != 1 || len(runner.Traces()) != 1 {
		t.Fatalf("trace mismatch: %#v, callback count %d", result.Trace, callbackCount)
	}
}

func TestOSRunnerTimeout(t *testing.T) {
	t.Parallel()
	runner := NewOSRunner(Config{Allowlist: Allowlist{Observe: []Rule{{Program: "sleep", Match: anyArgs}}}})
	result, err := runner.Run(context.Background(), Command{
		Program: "sleep", Args: []string{"1"}, Capability: CapabilityObserve,
		Risk: domain.RiskRead, Timeout: 5 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimedOut) || !result.Trace.TimedOut {
		t.Fatalf("Run() = %#v, %v; want timeout", result, err)
	}
}

func TestOSRunnerPreservesCallerCancellation(t *testing.T) {
	t.Parallel()
	runner := NewOSRunner(Config{Allowlist: Allowlist{Observe: []Rule{{Program: "sleep", Match: anyArgs}}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := runner.Run(ctx, Command{
		Program: "sleep", Args: []string{"1"}, Capability: CapabilityObserve,
		Risk: domain.RiskRead, Timeout: time.Second,
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimedOut) {
		t.Fatalf("Run() = %#v, %v; want caller cancellation, not command timeout", result, err)
	}
	if result.Trace.TimedOut || result.Trace.Error != context.Canceled.Error() {
		t.Fatalf("caller cancellation trace was misclassified: %#v", result.Trace)
	}
}

func TestOSRunnerPreservesCallerDeadline(t *testing.T) {
	t.Parallel()
	runner := NewOSRunner(Config{Allowlist: Allowlist{Observe: []Rule{{Program: "sleep", Match: anyArgs}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	result, err := runner.Run(ctx, Command{
		Program: "sleep", Args: []string{"1"}, Capability: CapabilityObserve,
		Risk: domain.RiskRead, Timeout: time.Second,
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimedOut) {
		t.Fatalf("Run() = %#v, %v; want caller deadline, not command timeout", result, err)
	}
	if !result.Trace.TimedOut || result.Trace.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("caller deadline trace was misclassified: %#v", result.Trace)
	}
}

func TestOSRunnerFailsClosedWhenCapturedOutputExceedsLimit(t *testing.T) {
	if _, err := ResolveProgram("head"); err != nil {
		t.Skip("head not available")
	}
	runner := NewOSRunner(Config{
		Allowlist:      Allowlist{Observe: []Rule{{Program: "head", Match: anyArgs}}},
		MaxStdoutBytes: 32,
		MaxStderrBytes: 32,
	})
	result, err := runner.Run(context.Background(), Command{
		Program: "head", Args: []string{"-c", "64", "/dev/zero"},
		Capability: CapabilityObserve, Risk: domain.RiskRead,
	})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("Run() error = %v, want ErrOutputLimit", err)
	}
	if len(result.Stdout) != 32 {
		t.Fatalf("captured stdout length = %d, want 32", len(result.Stdout))
	}
	if result.Trace.ExitCode != -1 || !strings.Contains(result.Trace.Error, ErrOutputLimit.Error()) {
		t.Fatalf("overflow did not fail the trace closed: %#v", result.Trace)
	}
}

func TestBoundedBufferConsumesWithoutRetainingOverflow(t *testing.T) {
	buffer := NewBoundedBuffer(4)
	if written, err := buffer.Write([]byte("abcdefgh")); err != nil || written != 8 {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if got := buffer.String(); got != "abcd" {
		t.Fatalf("String() = %q, want abcd", got)
	}
	if !errors.Is(buffer.LimitError("test"), ErrOutputLimit) {
		t.Fatal("overflow did not return ErrOutputLimit")
	}
}

func TestOSRunnerTimeoutTerminatesNoninteractiveDescendants(t *testing.T) {
	runner := NewOSRunner(Config{Allowlist: Allowlist{Observe: []Rule{{Program: "sh", Match: anyArgs}}}})
	started := time.Now()
	result, err := runner.Run(context.Background(), Command{
		Program: "sh", Args: []string{"-c", "sleep 30 & child=$!; echo $child; wait"},
		Capability: CapabilityObserve, Risk: domain.RiskRead, Timeout: 100 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimedOut) || time.Since(started) > 3*time.Second {
		t.Fatalf("descendant retained pipe beyond timeout: elapsed=%s error=%v", time.Since(started), err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(result.Stdout)))
	if err != nil {
		t.Fatalf("helper did not report child PID: output=%q error=%v", result.Stdout, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// A simultaneously terminated child can briefly remain a zombie while
	// the host's init process reaps it. Neither absence nor zombie can run.
	deadline := time.Now().Add(time.Second)
	for {
		data, readErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(readErr, os.ErrNotExist) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		_, rest, ok := strings.Cut(string(data), ") ")
		if ok && (strings.HasPrefix(rest, "Z ") || strings.HasPrefix(rest, "X ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subprocess %d survived timeout: %s", pid, data)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFakeRunnerCanceledContextDoesNotConsumeResponse(t *testing.T) {
	runner := NewFakeRunner(true, FakeResponse{Stdout: []byte("fresh")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := Command{Program: "zpool", Args: []string{"version", "-j"}, Capability: CapabilityObserve, Risk: domain.RiskRead}
	result, err := runner.Run(ctx, command)
	if !errors.Is(err, context.Canceled) || result.Trace.TimedOut || len(runner.Calls()) != 0 {
		t.Fatalf("canceled fake command dispatched or misclassified: %+v, %v", result, err)
	}
	result, err = runner.Run(context.Background(), command)
	if err != nil || string(result.Stdout) != "fresh" {
		t.Fatalf("canceled fake command consumed response: %+v, %v", result, err)
	}
}

func TestTraceCallbackCannotRewriteRecordedArgv(t *testing.T) {
	runner := NewOSRunner(Config{
		Allowlist: Allowlist{Observe: []Rule{{Program: "true", Match: anyArgs}}},
		OnTrace:   func(trace domain.CommandTrace) { trace.Args[0] = "rewritten" },
	})
	result, err := runner.Run(context.Background(), Command{Program: "true", Args: []string{"original"}, Capability: CapabilityObserve, Risk: domain.RiskRead})
	if err != nil {
		t.Fatal(err)
	}
	if result.Trace.Args[0] != "original" || runner.Traces()[0].Args[0] != "original" {
		t.Fatalf("callback rewrote recorded argv: result=%+v traces=%+v", result.Trace, runner.Traces())
	}
}
