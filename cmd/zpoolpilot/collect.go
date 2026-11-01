// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/discovery"
	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
	"github.com/bouldinnathan/zpoolpilot/internal/lockfile"
	"github.com/bouldinnathan/zpoolpilot/internal/monitor"
	"github.com/bouldinnathan/zpoolpilot/internal/store"
	"github.com/bouldinnathan/zpoolpilot/internal/terminal"
	"github.com/bouldinnathan/zpoolpilot/internal/tui"
)

type discoveryRunner struct{ runner execx.Runner }

func (r discoveryRunner) Run(ctx context.Context, command discovery.Command) discovery.Result {
	result, err := r.runner.Run(ctx, execx.Command{Program: command.Program, Args: append([]string(nil), command.Args...), Capability: execx.CapabilityObserve, Risk: domain.RiskRead, Timeout: command.Timeout})
	return discovery.Result{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.Trace.ExitCode, TimedOut: result.Trace.TimedOut, Err: err}
}

func (a *app) newRunner(readOnly bool) *execx.OSRunner {
	return execx.NewOSRunner(execx.Config{ReadOnly: readOnly, DefaultTimeout: a.cfg.CommandTimeout})
}

func (a *app) collect(ctx context.Context, profile string, wake, includeCommands bool) (domain.Snapshot, error) {
	if profile != "fast" && profile != "hardware" && profile != "all" {
		return domain.Snapshot{}, fmt.Errorf("invalid collection profile %q", profile)
	}
	runner := a.newRunner(true)
	opts := discovery.Options{CommandTimeout: a.cfg.CommandTimeout, IncludeCommandTrace: includeCommands, SkipSMART: profile == "fast", WakeSleepingDisks: wake, Thresholds: a.discoveryThresholds()}
	snapshot, err := discovery.New(discoveryRunner{runner: runner}).Collect(ctx, opts)
	if snapshot.APIVersion == "" {
		snapshot.APIVersion = domain.SnapshotAPIVersion
	}
	if snapshot.Generated.IsZero() {
		snapshot.Generated = time.Now().UTC()
	}
	if bayErr := inventory.MergeMappedBays(ctx, &snapshot, inventory.OpenBayFile(a.cfg.BayMapPath)); bayErr != nil {
		recordBayMappingError(&snapshot, bayErr)
		if errors.Is(bayErr, context.Canceled) || errors.Is(bayErr, context.DeadlineExceeded) {
			err = errors.Join(err, fmt.Errorf("authoritative bay mappings: %w", bayErr))
		}
	}
	if includeCommands || a.cfg.IncludeCommands {
		snapshot.Commands = runner.Traces()
	} else {
		snapshot.Commands = nil
	}
	appendTelemetryFreshnessFindings(&snapshot, time.Now().UTC(), 2*a.cfg.HardwareInterval)
	return snapshot, err
}

func (a *app) loadSnapshot(ctx context.Context) (domain.Snapshot, error) {
	bayStore := inventory.OpenBayFile(a.cfg.BayMapPath)
	snapshot, err := monitor.Load(a.cfg.SnapshotPath)
	if err == nil {
		if bayErr := inventory.MergeMappedBays(ctx, &snapshot, bayStore); bayErr != nil {
			recordBayMappingError(&snapshot, bayErr)
			if errors.Is(bayErr, context.Canceled) || errors.Is(bayErr, context.DeadlineExceeded) {
				return snapshot, fmt.Errorf("load authoritative bay mappings: %w", bayErr)
			}
		}
		appendTelemetryFreshnessFindings(&snapshot, time.Now().UTC(), 2*a.cfg.HardwareInterval)
		return snapshot, nil
	}
	state := store.Open(a.cfg.StatePath)
	fromDB, dbErr := state.LoadSnapshot(ctx)
	if dbErr == nil {
		if validateErr := monitor.ValidateSnapshot(fromDB, time.Now().UTC()); validateErr == nil {
			if bayErr := inventory.MergeMappedBays(ctx, &fromDB, bayStore); bayErr != nil {
				recordBayMappingError(&fromDB, bayErr)
				if errors.Is(bayErr, context.Canceled) || errors.Is(bayErr, context.DeadlineExceeded) {
					return fromDB, fmt.Errorf("load authoritative bay mappings: %w", bayErr)
				}
			}
			appendTelemetryFreshnessFindings(&fromDB, time.Now().UTC(), 2*a.cfg.HardwareInterval)
			return fromDB, nil
		} else {
			dbErr = validateErr
		}
	}
	return domain.Snapshot{}, errors.Join(err, dbErr)
}

// recordBayMappingError makes rejection of the separately authenticated bay
// store visible without restoring mappings from the untrusted snapshot cache.
// A missing optional store is filtered by MergeMappedBays before this helper.
func recordBayMappingError(snapshot *domain.Snapshot, err error) {
	if snapshot == nil || err == nil {
		return
	}
	for _, sourceError := range snapshot.Errors {
		if sourceError.Source == "inventory.bays" {
			return
		}
	}
	snapshot.Errors = append(snapshot.Errors, domain.SourceError{
		Source: "inventory.bays", Kind: "trust", Message: err.Error(), Partial: true,
	})
	snapshot.Partial = true
}

// collectAndSaveSnapshot holds the writer lock during discovery as well as
// persistence. Otherwise a slow collector can overwrite a newer snapshot
// collected and saved while the first collector was still probing hardware.
// A failed or canceled collection must leave the previous cache intact.
func (a *app) collectAndSaveSnapshot(ctx context.Context, preserveHardware, wait bool, collect func(context.Context) (domain.Snapshot, error)) (snapshot domain.Snapshot, collectErr, saveErr error) {
	lock, err := lockfile.Acquire(ctx, a.cfg.CollectionLockPath, wait)
	if err != nil {
		return snapshot, nil, fmt.Errorf("snapshot writer lock: %w", err)
	}
	defer lock.Close()
	snapshot, collectErr = collect(ctx)
	if collectErr != nil {
		return snapshot, collectErr, nil
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err, nil
	}
	saveErr = a.saveSnapshotLocked(ctx, &snapshot, preserveHardware)
	return snapshot, nil, saveErr
}

func (a *app) saveSnapshot(ctx context.Context, snapshot *domain.Snapshot, preserveHardware, wait bool) error {
	lock, err := lockfile.Acquire(ctx, a.cfg.CollectionLockPath, wait)
	if err != nil {
		return fmt.Errorf("snapshot writer lock: %w", err)
	}
	defer lock.Close()
	return a.saveSnapshotLocked(ctx, snapshot, preserveHardware)
}

// The caller holds the collection lock until both cache writes finish.
func (a *app) saveSnapshotLocked(ctx context.Context, snapshot *domain.Snapshot, preserveHardware bool) error {
	if snapshot == nil {
		return errors.New("cannot save a nil snapshot")
	}
	if previous, loadErr := monitor.Load(a.cfg.SnapshotPath); loadErr == nil {
		if previous.Generated.After(snapshot.Generated) {
			return errors.New("refusing to replace a newer cached snapshot with an older collection")
		}
		mergeDriveTelemetry(snapshot, previous, preserveHardware)
		if preserveHardware {
			mergeHardwareErrors(snapshot, previous)
		}
	}
	snapshot.Findings = discovery.EvaluateFindings(*snapshot, a.discoveryThresholds())
	inventory.DetectConflicts(snapshot)
	appendTelemetryFreshnessFindings(snapshot, time.Now().UTC(), 2*a.cfg.HardwareInterval)
	var errs []error
	if err := monitor.SaveAtomic(a.cfg.SnapshotPath, *snapshot); err != nil {
		errs = append(errs, fmt.Errorf("snapshot cache: %w", err))
	}
	state := store.Open(a.cfg.StatePath)
	if err := state.Init(ctx); err != nil {
		errs = append(errs, fmt.Errorf("state init: %w", err))
	} else if err := state.SaveSnapshot(ctx, *snapshot); err != nil {
		errs = append(errs, fmt.Errorf("state snapshot: %w", err))
	}
	return errors.Join(errs...)
}

func (a *app) discoveryThresholds() discovery.Thresholds {
	return discovery.Thresholds{
		CapacityWarningPercent: a.cfg.Thresholds.CapacityWarning, CapacityCriticalPercent: a.cfg.Thresholds.CapacityCritical,
		WearWarningPercent: a.cfg.Thresholds.WearWarning, WearCriticalPercent: a.cfg.Thresholds.WearCritical,
		TemperatureWarningC: a.cfg.Thresholds.TemperatureWarningC, TemperatureCriticalC: a.cfg.Thresholds.TemperatureCriticalC,
		FragmentationAdvisoryPercent: a.cfg.Thresholds.Fragmentation,
	}
}

func mergeDriveTelemetry(current *domain.Snapshot, previous domain.Snapshot, preserveHardware bool) {
	if current == nil {
		return
	}
	old := make(map[string]domain.Drive, len(previous.Drives))
	for _, drive := range previous.Drives {
		old[drive.ID] = drive
	}
	for index := range current.Drives {
		drive := &current.Drives[index]
		prior, ok := old[drive.ID]
		if !ok || drive.ID == "" || drive.WWN != prior.WWN || drive.Serial != prior.Serial || drive.CapacityBytes != prior.CapacityBytes {
			continue
		}
		if preserveHardware {
			drive.Health = prior.Health
			continue
		}
		if drive.Health.Sleeping && !prior.Health.FreshAt.IsZero() {
			message := drive.Health.Message
			drive.Health = prior.Health
			drive.Health.Sleeping = true
			if message != "" {
				drive.Health.Message = message + "; showing the last collected values"
			}
		}
	}
}

func mergeHardwareErrors(current *domain.Snapshot, previous domain.Snapshot) {
	if current == nil {
		return
	}
	present := make(map[string]bool, len(current.Drives))
	previousDrives := make(map[string]domain.Drive, len(previous.Drives))
	for _, drive := range previous.Drives {
		previousDrives[drive.ID] = drive
	}
	for _, drive := range current.Drives {
		prior, found := previousDrives[drive.ID]
		present[drive.ID] = found && drive.ID != "" && drive.WWN == prior.WWN &&
			drive.Serial == prior.Serial && drive.CapacityBytes == prior.CapacityBytes
	}
	existing := make(map[string]bool, len(current.Errors))
	for _, item := range current.Errors {
		existing[item.Source+"\x00"+item.Kind+"\x00"+item.Message] = true
	}
	for _, item := range previous.Errors {
		if !strings.HasPrefix(item.Source, "smart:") || !present[strings.TrimPrefix(item.Source, "smart:")] {
			continue
		}
		key := item.Source + "\x00" + item.Kind + "\x00" + item.Message
		if !existing[key] {
			current.Errors = append(current.Errors, item)
			existing[key] = true
		}
	}
	current.Partial = false
	for _, item := range current.Errors {
		if item.Partial {
			current.Partial = true
			break
		}
	}
}

func appendTelemetryFreshnessFindings(snapshot *domain.Snapshot, now time.Time, staleAfter time.Duration) {
	monitor.RefreshTelemetryFindings(snapshot, now, staleAfter)
}

// safetySnapshotError distinguishes optional telemetry loss (SMART, ARC,
// iostat, Proxmox probing) from sources needed to authorize storage changes.
// Unknown safety state is an error even when discovery can still render a
// useful partial dashboard.
func safetySnapshotError(snapshot domain.Snapshot) error {
	var reasons []string
	if snapshot.Host.MachineID == "" {
		reasons = append(reasons, "host machine identity is unavailable")
	}
	for _, sourceError := range snapshot.Errors {
		source := sourceError.Source
		required := source == "host.machine-id" || source == "host.root-mount" || source == "zpool.version" || source == "zpool.version.json" ||
			source == "zpool.status" || source == "zpool.status.json" || source == "zpool.list" || source == "zpool.list.json" ||
			source == "zpool.get" || source == "zpool.get.json" || source == "zfs.get" || source == "zfs.get.json" ||
			source == "lsblk" || source == "drive.by-id" || source == "drive.identity" || source == "drive.holders" || strings.HasPrefix(source, "udev:") || strings.HasPrefix(source, "drive.holders:")
		if required {
			reasons = append(reasons, fmt.Sprintf("%s (%s): %s", source, sourceError.Kind, sourceError.Message))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	if len(reasons) > 4 {
		reasons = append(reasons[:4], fmt.Sprintf("and %d more safety-source errors", len(reasons)-4))
	}
	return fmt.Errorf("storage changes are blocked because safety discovery is incomplete: %s", strings.Join(reasons, "; "))
}

// callerContextError recovers the caller's cancellation cause after a
// best-effort operation has produced useful partial output. Discovery reserves
// context cancellation for its returned error, but checking ctx as well closes
// the small window where cancellation arrives after collection completes.
func callerContextError(ctx context.Context, operationErr error) error {
	var cause error
	if ctx != nil {
		cause = ctx.Err()
	}
	if cause == nil {
		switch {
		case errors.Is(operationErr, context.Canceled):
			cause = context.Canceled
		case errors.Is(operationErr, context.DeadlineExceeded):
			cause = context.DeadlineExceeded
		}
	}
	if cause == nil {
		return nil
	}
	return fmt.Errorf("caller context ended: %w", cause)
}

func (a *app) runStatus(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	jsonOut := fs.Bool("json", false, "emit SnapshotV1 JSON")
	refresh := fs.Bool("refresh", false, "collect a fresh snapshot")
	include := fs.Bool("include-commands", a.cfg.IncludeCommands, "include redacted read command traces")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 {
		return 64, errors.New("status takes no positional arguments")
	}
	var snapshot domain.Snapshot
	var err error
	if *refresh {
		snapshot, err = a.collect(ctx, "all", false, *include)
	} else {
		snapshot, err = a.loadSnapshot(ctx)
		if err != nil {
			snapshot, err = a.collect(ctx, "fast", false, *include)
		}
	}
	if err != nil && snapshot.APIVersion == "" {
		return 3, err
	}
	contextErr := callerContextError(ctx, err)
	if !*include {
		snapshot.Commands = nil
	}
	if *jsonOut {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		if encodeErr := enc.Encode(snapshot); encodeErr != nil {
			return 1, errors.Join(encodeErr, contextErr)
		}
	} else {
		tui.Summary(a.out, snapshot, a.color)
	}
	if err != nil {
		fmt.Fprintln(a.errOut, "zpoolpilot: collection was partial:", terminalText(err.Error()))
	}
	if contextErr != nil {
		return monitor.ExitCode(snapshot), fmt.Errorf("status interrupted: %w", contextErr)
	}
	return monitor.ExitCode(snapshot), nil
}

func (a *app) runTUI(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	refresh := fs.Bool("refresh", false, "collect before opening")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 {
		return 64, errors.New("tui takes no positional arguments")
	}
	snapshot, err := a.loadSnapshot(ctx)
	if *refresh || err != nil {
		snapshot, err = a.collectFastView(ctx)
	}
	if err != nil && snapshot.APIVersion == "" {
		return 3, err
	}
	refreshFn := a.collectFastView
	return 0, tui.Run(ctx, snapshot, tui.Options{In: a.in, Out: a.out, Color: a.color, ReadOnly: a.readOnly, HardwareMaxAge: 2 * a.cfg.HardwareInterval, Refresh: refreshFn})
}

// A fast dashboard refresh updates topology without discarding the last SMART
// readings. Cache data is used only for this observer view; planning and apply
// continue to collect their own safety snapshots directly.
func (a *app) collectFastView(ctx context.Context) (domain.Snapshot, error) {
	snapshot, err := a.collect(ctx, "fast", false, true)
	if err != nil {
		return snapshot, err
	}
	if previous, loadErr := a.loadSnapshot(ctx); loadErr == nil {
		mergeDriveTelemetry(&snapshot, previous, true)
		mergeHardwareErrors(&snapshot, previous)
		snapshot.Findings = discovery.EvaluateFindings(snapshot, a.discoveryThresholds())
		inventory.DetectConflicts(&snapshot)
	}
	appendTelemetryFreshnessFindings(&snapshot, time.Now().UTC(), 2*a.cfg.HardwareInterval)
	return snapshot, callerContextError(ctx, nil)
}

func (a *app) runMonitor(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 64, errors.New("monitor requires collect or serve")
	}
	switch args[0] {
	case "collect":
		fs := flag.NewFlagSet("monitor collect", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		profile := fs.String("profile", "all", "fast, hardware, or all")
		wake := fs.Bool("wake", false, "allow SMART collection to wake standby disks")
		wait := fs.Bool("wait", false, "wait for another collector to finish")
		include := fs.Bool("include-commands", a.cfg.IncludeCommands, "cache redacted command traces")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 {
			return 64, errors.New("monitor collect takes no positional arguments")
		}
		if *profile != "fast" && *profile != "hardware" && *profile != "all" {
			return 64, fmt.Errorf("invalid collection profile %q", *profile)
		}
		if *wake && *profile == "fast" {
			return 64, errors.New("--wake requires the hardware or all collection profile")
		}
		if *wake {
			if a.readOnly {
				return 77, errors.New("--wake is unavailable in read-only mode")
			}
			ok, err := a.confirmYellowContext(ctx, "SMART queries may spin up sleeping disks", []string{"smartctl", "-j", "-x", "DEVICE"})
			if err != nil {
				return 1, err
			}
			if !ok {
				return 1, errors.New("deep refresh declined")
			}
		}
		snapshot, collectErr, saveErr := a.collectAndSaveSnapshot(ctx, *profile == "fast", *wait, func(callCtx context.Context) (domain.Snapshot, error) {
			return a.collect(callCtx, *profile, *wake, *include)
		})
		if collectErr != nil {
			return 69, fmt.Errorf("collection did not complete: %w", collectErr)
		}
		if saveErr != nil {
			// Codes 1-3 are reserved for successfully persisted health states and
			// are declared successful by the packaged systemd collector units.
			return 74, saveErr
		}
		fmt.Fprintf(a.out, "snapshot %s written to %s (%d pools, %d drives, partial=%t)\n", snapshot.Generated.Format(time.RFC3339), terminalText(a.cfg.SnapshotPath), len(snapshot.Pools), len(snapshot.Drives), snapshot.Partial)
		return monitor.ExitCode(snapshot), nil
	case "serve":
		fs := flag.NewFlagSet("monitor serve", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		listen := fs.String("listen", a.cfg.ListenAddress, "listen address")
		maxAge := fs.Duration("max-age", 2*a.cfg.HardwareInterval, "readiness snapshot maximum age")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 {
			return 64, errors.New("monitor serve takes no positional arguments")
		}
		if *maxAge <= 0 {
			return 64, errors.New("--max-age must be positive")
		}
		fmt.Fprintf(a.errOut, "zpoolpilot: serving cached read-only metrics on http://%s/metrics\n", terminalText(*listen))
		return 0, monitor.Serve(ctx, *listen, a.cfg.SnapshotPath, *maxAge, 2*a.cfg.HardwareInterval)
	default:
		return 64, fmt.Errorf("unknown monitor command %q", args[0])
	}
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func isTTY(file *os.File) bool {
	return terminal.IsTerminal(file)
}
