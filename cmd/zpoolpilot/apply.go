// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

type mutationAuditRecord struct {
	At          time.Time             `json:"at"`
	Operator    string                `json:"operator"`
	Host        string                `json:"host"`
	PlanID      string                `json:"plan_id"`
	Fingerprint string                `json:"fingerprint"`
	Operation   string                `json:"operation"`
	Result      string                `json:"result"`
	Commands    []domain.CommandTrace `json:"commands,omitempty"`
	Before      string                `json:"before_fingerprint,omitempty"`
	After       string                `json:"after_fingerprint,omitempty"`
	Message     string                `json:"message,omitempty"`
}

func (a *app) runApply(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	timeout := fs.Duration("command-timeout", 10*time.Minute, "per-command timeout")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 1 {
		return 64, errors.New("usage: zpoolpilot apply PLAN.json")
	}
	if a.readOnly {
		return 77, execx.ErrReadOnly
	}
	if os.Geteuid() != 0 {
		return 77, errors.New("apply must run as root; inspect the plan first, then use the displayed sudo command")
	}
	if !isTTY(a.in) {
		return 77, planner.ErrTTYRequired
	}
	plan, pathErr := planner.LoadPlan(fs.Arg(0))
	if pathErr != nil {
		return 2, pathErr
	}
	if err := validatePlanKeyFile(plan); err != nil {
		return 2, err
	}
	if _, err := fmt.Fprintf(a.out, "Applying plan %s (%s), expires %s\n", terminalText(plan.ID), terminalText(plan.Operation), plan.ExpiresAt.Local().Format(time.RFC3339)); err != nil {
		return 1, fmt.Errorf("display plan: %w", err)
	}
	for i, c := range plan.Commands {
		if _, err := fmt.Fprintf(a.out, "%d. %s %s\n   %s\n", i+1, riskLabel(a.color, c.Risk), displayCommand(c.Program, c.Args), terminalText(c.Reason)); err != nil {
			return 1, fmt.Errorf("display planned command: %w", err)
		}
	}
	if len(plan.Warnings) > 0 {
		for _, w := range plan.Warnings {
			if _, err := fmt.Fprintf(a.out, "%s %s\n", riskLabel(a.color, domain.RiskChange), terminalText(w)); err != nil {
				return 1, fmt.Errorf("display plan warning: %w", err)
			}
		}
	}
	runner := a.newRunner(false)
	profile := "fast"
	if operationUsesTargetDiskHealth(planner.Operation(plan.Operation)) {
		profile = "all"
	}
	fresh := func(callCtx context.Context) (domain.Snapshot, error) {
		// A key path is part of an encrypted create plan, but the filesystem
		// object can change after planning or while confirmation is pending.
		// Recheck it on every safety refresh, including immediately before the
		// mutating command.
		if err := validatePlanKeyFile(plan); err != nil {
			return domain.Snapshot{}, err
		}
		// SMART and ARC are not safety inputs. Avoid disk wake-ups and refuse
		// only when topology/identity sources are incomplete.
		snapshot, err := a.collect(callCtx, profile, false, true)
		if err != nil {
			return snapshot, err
		}
		if err := safetySnapshotError(snapshot); err != nil {
			return snapshot, err
		}
		return snapshot, nil
	}
	audit := func(callCtx context.Context, record planner.AuditRecord) error {
		return a.writeAudit(callCtx, record)
	}
	confirm := planner.Confirmations{
		Green: func(_ context.Context, _ planner.ConfirmationRequest) (string, error) { return "", nil },
		Yellow: func(callCtx context.Context, r planner.ConfirmationRequest) (string, error) {
			line, err := a.readConfirmation(callCtx, "\n%s\n  %s\nReason: %s\nApprove [Y/n]: ", riskLabel(a.color, r.Command.Risk), displayCommand(r.Command.Program, r.Command.Args), terminalText(r.Command.Reason))
			if err != nil {
				return "", err
			}
			return trimLine(line), nil
		},
		Red: func(callCtx context.Context, r planner.ConfirmationRequest) (string, error) {
			return a.promptExact(callCtx, r.Command.Risk, r.Command.Reason, r.Command.Program, r.Command.Args, r.Expected)
		},
	}
	result, err := planner.Apply(ctx, plan, planner.ApplyOptions{Runner: runner, IsTTY: func() bool { return isTTY(a.in) }, LockPath: a.cfg.LockPath, FreshSnapshot: fresh, Confirm: confirm, BeforeAudit: audit, AfterAudit: audit, CommandGuard: guardPlannedCommand, CommandTimeout: *timeout})
	if !result.After.Generated.IsZero() {
		if saveErr := a.saveSnapshot(ctx, &result.After, profile == "fast", true); saveErr != nil {
			fmt.Fprintln(a.errOut, "zpoolpilot: warning: could not cache the result snapshot:", terminalText(saveErr.Error()))
		}
	}
	if err != nil {
		return 2, err
	}
	fmt.Fprintf(a.out, "%s plan %s completed; before=%s after=%s\n", riskLabel(a.color, domain.RiskRead), terminalText(plan.ID), planner.SnapshotFingerprint(result.Before)[:12], planner.SnapshotFingerprint(result.After)[:12])
	return 0, nil
}

func (a *app) writeAudit(_ context.Context, record planner.AuditRecord) error {
	operator := os.Getenv("SUDO_USER")
	if operator == "" {
		if current, err := user.Current(); err == nil {
			operator = current.Username
		}
	}
	host := ""
	before, after := "", ""
	if record.Before != nil {
		host = record.Before.Host.Hostname
		before = planner.SnapshotFingerprint(*record.Before)
	}
	if record.After != nil {
		if host == "" {
			host = record.After.Host.Hostname
		}
		after = planner.SnapshotFingerprint(*record.After)
	}
	stored := mutationAuditRecord{At: record.At, Operator: operator, Host: host, PlanID: record.PlanID, Fingerprint: record.Fingerprint, Operation: string(record.Operation), Result: record.Phase, Commands: record.Commands, Before: before, After: after, Message: record.Error}
	file, err := openAuditFile(a.cfg.AuditPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encodeErr := encoder.Encode(stored)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(encodeErr, syncErr, closeErr)
}

// openAuditFile pins a root/operator-owned, singly-linked regular file beneath
// a non-writable directory. Unlike the shared monitoring database, this log is
// authoritative for mutation attempts and is never writable by the exporter.
func openAuditFile(path string) (*os.File, error) {
	if os.Geteuid() == 0 && !pathWithin(path, "/var/log/zpoolpilot") {
		return nil, errors.New("privileged audit path must remain beneath /var/log/zpoolpilot")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		if err == nil {
			err = errors.New("directory contains a symbolic link")
		}
		return nil, fmt.Errorf("unsafe audit directory %q: %w", directory, err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect audit directory: %w", err)
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !directoryInfo.IsDir() || !ok || directoryInfo.Mode().Perm()&0o022 != 0 || directoryStat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("audit directory %q must be owned by uid %d and not writable by group/other", directory, os.Geteuid())
	}
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_APPEND|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fail(fmt.Errorf("stat audit log: %w", err))
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return fail(fmt.Errorf("audit log must be a singly linked regular file owned by uid %d", os.Geteuid()))
	}
	if stat.Mode&0o022 != 0 {
		return fail(errors.New("audit log must not be writable by group/other"))
	}
	if err := syscall.Fchmod(fd, 0o640); err != nil {
		return fail(fmt.Errorf("set audit log permissions: %w", err))
	}
	return file, nil
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
