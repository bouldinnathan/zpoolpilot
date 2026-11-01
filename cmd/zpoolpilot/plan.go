// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/bouldinnathan/zpoolpilot/internal/discovery"
	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

type repeatedFlag []string

func (r *repeatedFlag) String() string         { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(value string) error { *r = append(*r, value); return nil }

func (a *app) runPlan(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 64, errors.New("plan requires an operation")
	}
	// Plans are always based on a fresh, non-waking safety snapshot. A cached
	// dashboard is useful for monitoring but is not authority to select disks.
	profile := "fast"
	if operationUsesTargetDiskHealth(planner.Operation(args[0])) {
		// Disk-consuming operations collect SMART with explicit device types and guarded power checks. This
		// rejects a known-failing target without waking a sleeping disk; missing
		// or sleeping telemetry remains an explicit warning in the plan.
		profile = "all"
	}
	snapshot, err := a.collect(ctx, profile, false, true)
	if err != nil {
		return 3, fmt.Errorf("fresh safety discovery failed: %w", err)
	}
	if err := safetySnapshotError(snapshot); err != nil {
		return 3, err
	}
	var plan domain.ActionPlan
	var out string
	switch args[0] {
	case "create-pool":
		fs := flag.NewFlagSet("plan create-pool", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		pool := fs.String("pool", "", "new pool name")
		topology := fs.String("topology", "", "single, mirror, raidz1, raidz2, or raidz3")
		drives := fs.String("drives", "", "comma-separated stable drive IDs")
		profile := fs.String("profile", "general", "general or proxmox")
		expert := fs.Bool("expert", false, "allow selected limited-redundancy/mixed-capacity choices")
		encrypt := fs.String("encrypt", "off", "off, passphrase, or keyfile")
		keyfile := fs.String("keyfile", "", "absolute existing keyfile path")
		keyFormat := fs.String("key-format", "passphrase", "passphrase, raw, or hex")
		fs.StringVar(&out, "out", "", "plan output path")
		var poolProps, datasetProps repeatedFlag
		fs.Var(&poolProps, "property", "pool property key=value (repeatable)")
		fs.Var(&datasetProps, "dataset-property", "root dataset property key=value (repeatable)")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *pool == "" || *topology == "" || *drives == "" {
			return 64, errors.New("usage: plan create-pool --pool NAME --topology TYPE --drives ID,ID [options]")
		}
		pp, err := propertyMap(poolProps)
		if err != nil {
			return 64, err
		}
		dp, err := propertyMap(datasetProps)
		if err != nil {
			return 64, err
		}
		switch *profile {
		case "general":
		case "proxmox":
			if _, ok := dp["atime"]; !ok {
				dp["atime"] = "off"
			}
			if _, ok := dp["xattr"]; !ok {
				dp["xattr"] = "sa"
			}
		default:
			return 64, fmt.Errorf("unknown profile %q", *profile)
		}
		switch *encrypt {
		case "off":
		case "passphrase":
			dp["encryption"] = "on"
			dp["keyformat"] = "passphrase"
			dp["keylocation"] = "prompt"
		case "keyfile":
			if !filepath.IsAbs(*keyfile) {
				return 64, errors.New("--keyfile must be absolute")
			}
			if err := validateKeyFile(*keyfile); err != nil {
				return 64, err
			}
			if *keyFormat != "passphrase" && *keyFormat != "raw" && *keyFormat != "hex" {
				return 64, errors.New("--key-format must be passphrase, raw, or hex")
			}
			dp["encryption"] = "on"
			dp["keyformat"] = *keyFormat
			dp["keylocation"] = "file://" + *keyfile
		default:
			return 64, fmt.Errorf("unknown --encrypt mode %q", *encrypt)
		}
		selected, err := selectDrives(snapshot, *drives)
		if err != nil {
			return 2, err
		}
		plan, err = planner.NewCreatePoolPlan(planner.CreatePoolRequest{Snapshot: snapshot, PoolName: *pool, Topology: planner.Topology(*topology), Drives: selected, PoolProperties: pp, DatasetProperties: dp, Expert: *expert})
		if err != nil {
			return 2, err
		}
	case "prepare-disk":
		fs := flag.NewFlagSet("plan prepare-disk", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		driveID := fs.String("drive", "", "stable drive ID")
		fs.StringVar(&out, "out", "", "plan output path")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *driveID == "" {
			return 64, errors.New("usage: plan prepare-disk --drive ID")
		}
		drive, ok := inventory.FindDrive(snapshot, *driveID)
		if !ok {
			return 2, fmt.Errorf("drive %q did not resolve uniquely", *driveID)
		}
		plan, err = planner.NewPrepareDiskPlan(planner.PrepareDiskRequest{Snapshot: snapshot, Drive: drive})
	case "scrub-start", "scrub-stop", "trim-start", "trim-stop", "clear-errors":
		fs := flag.NewFlagSet("plan "+args[0], flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		poolID := fs.String("pool", "", "pool name or GUID")
		fs.StringVar(&out, "out", "", "plan output path")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *poolID == "" {
			return 64, fmt.Errorf("usage: plan %s --pool POOL", args[0])
		}
		pool, ok := inventory.FindPool(snapshot, *poolID)
		if !ok {
			return 2, fmt.Errorf("pool %q not found", *poolID)
		}
		operation, ok := maintenancePlanOperation(args[0])
		if !ok {
			return 64, fmt.Errorf("unknown maintenance operation %q", args[0])
		}
		plan, err = planner.NewMaintenancePlan(operation, planner.MaintenanceRequest{Snapshot: snapshot, Pool: pool})
	case "replace-boot-disk":
		fs := flag.NewFlagSet("plan replace-boot-disk", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		poolID := fs.String("pool", "", "boot pool name or GUID")
		vdevID := fs.String("vdev", "", "failed/old ZFS leaf name or GUID")
		sourceID := fs.String("source-drive", "", "healthy mirrored boot drive stable ID")
		driveID := fs.String("drive", "", "prepared replacement whole-disk stable ID")
		bootMode := fs.String("boot-mode", "", "systemd-boot or grub, verified from proxmox-boot-tool status")
		fs.StringVar(&out, "out", "", "plan output path")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *poolID == "" || *vdevID == "" || *sourceID == "" || *driveID == "" || *bootMode == "" {
			return 64, errors.New("usage: plan replace-boot-disk --pool POOL --vdev OLD --source-drive HEALTHY --drive NEW --boot-mode systemd-boot|grub")
		}
		mode := planner.BootMode(*bootMode)
		if mode != planner.BootModeSystemdBoot && mode != planner.BootModeGRUB {
			return 64, errors.New("--boot-mode must be systemd-boot or grub; plain GRUB is not supported")
		}
		if err := a.verifyProxmoxBootMode(ctx, mode); err != nil {
			return 2, err
		}
		pool, ok := inventory.FindPool(snapshot, *poolID)
		if !ok {
			return 2, fmt.Errorf("boot pool %q not found", *poolID)
		}
		vdev, ok := inventory.FindVDev(pool, *vdevID)
		if !ok {
			return 2, fmt.Errorf("old boot vdev %q did not resolve uniquely in %s", *vdevID, pool.Name)
		}
		source, ok := inventory.FindDrive(snapshot, *sourceID)
		if !ok {
			return 2, fmt.Errorf("source drive %q did not resolve uniquely", *sourceID)
		}
		drive, ok := inventory.FindDrive(snapshot, *driveID)
		if !ok {
			return 2, fmt.Errorf("replacement drive %q did not resolve uniquely", *driveID)
		}
		plan, err = planner.NewReplaceBootDiskPlan(planner.ReplaceBootDiskRequest{
			Snapshot: snapshot, Pool: pool, OldVDev: vdev, SourceDrive: source, NewDrive: drive, BootMode: mode,
		})
	case "replace-disk", "attach-mirror", "expand-raidz":
		fs := flag.NewFlagSet("plan "+args[0], flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		poolID := fs.String("pool", "", "pool name or GUID")
		vdevID := fs.String("vdev", "", "vdev name or GUID")
		driveID := fs.String("drive", "", "new stable drive ID")
		allowBoot := fs.Bool("allow-boot", false, "request boot-critical workflow (generic operations remain blocked)")
		fs.StringVar(&out, "out", "", "plan output path")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *poolID == "" || *vdevID == "" || *driveID == "" {
			return 64, fmt.Errorf("usage: plan %s --pool POOL --vdev VDEV --drive ID", args[0])
		}
		pool, ok := inventory.FindPool(snapshot, *poolID)
		if !ok {
			return 2, fmt.Errorf("pool %q not found", *poolID)
		}
		vdev, ok := inventory.FindVDev(pool, *vdevID)
		if !ok {
			return 2, fmt.Errorf("vdev %q did not resolve uniquely in %s", *vdevID, pool.Name)
		}
		drive, ok := inventory.FindDrive(snapshot, *driveID)
		if !ok {
			return 2, fmt.Errorf("drive %q did not resolve uniquely", *driveID)
		}
		switch args[0] {
		case "replace-disk":
			plan, err = planner.NewReplaceDiskPlan(planner.ReplaceDiskRequest{Snapshot: snapshot, Pool: pool, OldVDev: vdev, NewDrive: drive, AllowBoot: *allowBoot})
		case "attach-mirror":
			plan, err = planner.NewAttachMirrorPlan(planner.AttachMirrorRequest{Snapshot: snapshot, Pool: pool, ExistingVDev: vdev, NewDrive: drive, AllowBoot: *allowBoot})
		case "expand-raidz":
			plan, err = planner.NewExpandRAIDZPlan(planner.ExpandRAIDZRequest{Snapshot: snapshot, Pool: pool, RAIDZVDev: vdev, NewDrive: drive, AllowBoot: *allowBoot})
		}
	case "add-vdev":
		fs := flag.NewFlagSet("plan add-vdev", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		poolID := fs.String("pool", "", "pool name or GUID")
		topology := fs.String("topology", "", "matching vdev topology")
		driveIDs := fs.String("drives", "", "comma-separated stable drive IDs")
		allowBoot := fs.Bool("allow-boot", false, "request boot-critical workflow")
		fs.StringVar(&out, "out", "", "plan output path")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *poolID == "" || *topology == "" || *driveIDs == "" {
			return 64, errors.New("usage: plan add-vdev --pool POOL --topology TYPE --drives ID,ID")
		}
		pool, ok := inventory.FindPool(snapshot, *poolID)
		if !ok {
			return 2, fmt.Errorf("pool %q not found", *poolID)
		}
		selected, err := selectDrives(snapshot, *driveIDs)
		if err != nil {
			return 2, err
		}
		plan, err = planner.NewAddVDevPlan(planner.AddVDevRequest{Snapshot: snapshot, Pool: pool, Topology: planner.Topology(*topology), Drives: selected, AllowBoot: *allowBoot})
	case "add-spare":
		fs := flag.NewFlagSet("plan add-spare", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		poolID := fs.String("pool", "", "pool name or GUID")
		driveIDs := fs.String("drives", "", "comma-separated stable drive IDs")
		allowBoot := fs.Bool("allow-boot", false, "request boot-critical workflow")
		fs.StringVar(&out, "out", "", "plan output path")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 || *poolID == "" || *driveIDs == "" {
			return 64, errors.New("usage: plan add-spare --pool POOL --drives ID,ID")
		}
		pool, ok := inventory.FindPool(snapshot, *poolID)
		if !ok {
			return 2, fmt.Errorf("pool %q not found", *poolID)
		}
		selected, err := selectDrives(snapshot, *driveIDs)
		if err != nil {
			return 2, err
		}
		plan, err = planner.NewAddSparePlan(planner.AddSpareRequest{Snapshot: snapshot, Pool: pool, Drives: selected, AllowBoot: *allowBoot})
	default:
		return 64, fmt.Errorf("unknown plan operation %q", args[0])
	}
	if err != nil {
		return 2, err
	}
	return a.finishPlan(plan, out)
}

func operationUsesTargetDiskHealth(operation planner.Operation) bool {
	switch operation {
	case planner.OperationCreatePool, planner.OperationReplaceDisk,
		planner.OperationAttachMirror, planner.OperationExpandRAIDZ,
		planner.OperationAddVDev, planner.OperationAddSpare,
		planner.OperationReplaceBootDisk:
		return true
	default:
		return false
	}
}

func maintenancePlanOperation(value string) (planner.Operation, bool) {
	switch value {
	case string(planner.OperationScrubStart):
		return planner.OperationScrubStart, true
	case string(planner.OperationScrubStop):
		return planner.OperationScrubStop, true
	case string(planner.OperationTrimStart):
		return planner.OperationTrimStart, true
	case string(planner.OperationTrimStop):
		return planner.OperationTrimStop, true
	case string(planner.OperationClearErrors):
		return planner.OperationClearErrors, true
	default:
		return "", false
	}
}

func (a *app) verifyProxmoxBootMode(ctx context.Context, requested planner.BootMode) error {
	runner := a.newRunner(true)
	command := execx.Command{Program: "proxmox-boot-tool", Args: []string{"status"}, Capability: execx.CapabilityObserve, Risk: domain.RiskRead, Timeout: a.cfg.CommandTimeout}
	result, err := runner.Run(ctx, command)
	fmt.Fprintf(a.out, "%s %s\n", riskLabel(a.color, domain.RiskRead), displayCommand(command.Program, command.Args))
	combined := strings.TrimSpace(strings.TrimSpace(string(result.Stdout)) + "\n" + strings.TrimSpace(string(result.Stderr)))
	if len(combined) > 8192 {
		combined = combined[:8192] + "\n[output truncated]"
	}
	if combined != "" {
		fmt.Fprintln(a.out, indent(terminalText(combined), "  "))
	}
	if err != nil {
		return fmt.Errorf("cannot verify Proxmox boot mode: %w", err)
	}
	detected, err := detectProxmoxBootMode(combined)
	if err != nil {
		return err
	}
	if requested != detected {
		return fmt.Errorf("--boot-mode %s does not match proxmox-boot-tool status (%s); no repair or mode conversion is performed automatically", requested, detected)
	}
	return nil
}

func detectProxmoxBootMode(output string) (planner.BootMode, error) {
	mode, err := discovery.ParseProxmoxBootMode(output)
	if err != nil {
		return "", fmt.Errorf("%w; resolve mixed/unknown ESP state before replacement", err)
	}
	return planner.BootMode(mode), nil
}

func validateKeyFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect --keyfile: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("--keyfile must be a regular file, not a symlink or device")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("--keyfile must be a singly linked regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("--keyfile permissions %04o expose key material to group/other; use chmod 0600 or stricter", info.Mode().Perm())
	}
	if info.Size() <= 0 || info.Size() > 1<<20 {
		return errors.New("--keyfile must contain between 1 byte and 1 MiB")
	}
	return nil
}

// validatePrivilegedKeyFile ensures an unprivileged account cannot swap or
// rewrite a key between the final safety refresh and zpool opening it. Root is
// the only accepted owner, and every directory component must be root-owned
// and non-writable by group/other.
func validatePrivilegedKeyFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("privileged --keyfile path must be absolute and normalized")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect privileged --keyfile: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("privileged --keyfile must be owned by root")
	}
	directory := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve privileged --keyfile directory: %w", err)
	}
	if resolved != directory {
		return errors.New("privileged --keyfile directory path must not contain symbolic links")
	}
	for current := directory; ; current = filepath.Dir(current) {
		directoryInfo, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect privileged --keyfile directory %q: %w", current, err)
		}
		directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
		if !directoryInfo.IsDir() || !ok || directoryStat.Uid != 0 || directoryInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("privileged --keyfile directory %q must be root-owned and not writable by group/other", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

// validatePlanKeyFile revalidates the live filesystem object referenced by an
// encrypted create-pool plan. Plan validation separately constrains the
// property syntax; this check prevents a safe keyfile from being replaced by a
// symlink, device, empty file, or broadly readable file before execution.
func validatePlanKeyFile(plan domain.ActionPlan) error {
	if planner.Operation(plan.Operation) != planner.OperationCreatePool {
		return nil
	}
	raw, ok := plan.Desired["dataset_properties"]
	if !ok {
		return nil
	}
	properties := make(map[string]string)
	switch value := raw.(type) {
	case map[string]string:
		for key, property := range value {
			properties[key] = property
		}
	case map[string]any:
		for key, property := range value {
			text, ok := property.(string)
			if !ok {
				return fmt.Errorf("planned dataset property %q is not text", key)
			}
			properties[key] = text
		}
	default:
		return errors.New("planned dataset properties have an invalid representation")
	}
	location := properties["keylocation"]
	if location == "" || location == "prompt" || properties["encryption"] == "" || properties["encryption"] == "off" {
		return nil
	}
	if !strings.HasPrefix(location, "file://") {
		return fmt.Errorf("planned encryption keylocation %q is not a local file", location)
	}
	path := strings.TrimPrefix(location, "file://")
	if err := validateKeyFile(path); err != nil {
		return fmt.Errorf("planned encryption keyfile %q is no longer safe: %w", path, err)
	}
	if os.Geteuid() == 0 {
		if err := validatePrivilegedKeyFile(path); err != nil {
			return fmt.Errorf("planned encryption keyfile %q is not trusted for privileged use: %w", path, err)
		}
	}
	return nil
}

func (a *app) finishPlan(plan domain.ActionPlan, out string) (int, error) {
	fmt.Fprintf(a.out, "Plan %s  operation=%s  expires=%s\n", terminalText(plan.ID), terminalText(plan.Operation), plan.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST"))
	if plan.TargetPool.Name != "" {
		fmt.Fprintf(a.out, "Pool: %s", terminalText(plan.TargetPool.Name))
		if plan.TargetPool.GUID != "" {
			fmt.Fprintf(a.out, "  GUID=%s", terminalText(plan.TargetPool.GUID))
		}
		fmt.Fprintln(a.out)
	}
	if plan.TargetVDev.Name != "" || plan.TargetVDev.GUID != "" {
		fmt.Fprintf(a.out, "Vdev: %s  GUID=%s\n", valueOrUnknown(plan.TargetVDev.Name), valueOrUnknown(plan.TargetVDev.GUID))
	}
	for _, drive := range plan.SourceDrives {
		printPlanDrive(a.out, "SOURCE (read from)", drive)
	}
	for _, drive := range plan.TargetDrives {
		printPlanDrive(a.out, "TARGET (will change)", drive)
	}
	for _, b := range plan.Blockers {
		fmt.Fprintf(a.out, "%s BLOCKER: %s\n", riskLabel(a.color, domain.RiskDestructive), terminalText(b))
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(a.out, "%s WARNING: %s\n", riskLabel(a.color, domain.RiskChange), terminalText(w))
	}
	for i, c := range plan.Commands {
		fmt.Fprintf(a.out, "%d. %s %s\n   %s\n", i+1, riskLabel(a.color, c.Risk), displayCommand(c.Program, c.Args), terminalText(c.Reason))
	}
	fmt.Fprintf(a.out, "Expected result: %s\n", terminalText(plan.ExpectedResult))
	if plan.Risk == domain.RiskChange {
		fmt.Fprintln(a.out, "Confirmation: [Y/n] (Enter=yes at apply time)")
	} else {
		fmt.Fprintf(a.out, "Confirmation challenge: %q\n", terminalText(plan.Confirmation))
	}
	if out == "" {
		idPrefix := plan.ID
		if len(idPrefix) > 8 {
			idPrefix = idPrefix[:8]
		}
		filename := fmt.Sprintf("zpoolpilot-%s-%s.plan.json", plan.Operation, idPrefix)
		out = filename
		if os.Geteuid() == 0 {
			out = filepath.Join(a.cfg.PlanDir, filename)
		}
	}
	if os.Geteuid() == 0 {
		absolute, err := filepath.Abs(out)
		if err != nil {
			return 1, err
		}
		if !pathWithin(absolute, a.cfg.PlanDir) {
			return 77, fmt.Errorf("privileged plan output must remain beneath %s", a.cfg.PlanDir)
		}
		out = absolute
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return 1, err
	}
	if err := planner.SavePlan(out, plan); err != nil {
		return 2, err
	}
	absolute, _ := filepath.Abs(out)
	fmt.Fprintf(a.out, "Saved: %s\n", terminalText(absolute))
	fmt.Fprintf(a.out, "Apply from a trusted terminal:\n  sudo zpoolpilot apply %s\n", shellQuote(absolute))
	if len(plan.Blockers) > 0 {
		return 2, errors.New("plan is blocked and cannot be applied")
	}
	return 0, nil
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func printPlanDrive(out *os.File, role string, drive domain.DriveRef) {
	bay := valueOrUnknown(drive.Bay)
	serial := valueOrUnknown(drive.Serial)
	fmt.Fprintf(out, "%s: bay=%s  serial=%s  size=%s\n  stable-id=%s\n  path=%s  device=%s\n",
		role, bay, serial, humanBytes(drive.CapacityBytes), valueOrUnknown(drive.ID), valueOrUnknown(drive.ByID), valueOrUnknown(drive.MajorMinor))
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "UNKNOWN"
	}
	return terminalText(value)
}

func humanBytes(value uint64) string {
	const unit = uint64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor := unit
	unitIndex := 0
	for value/divisor >= unit && unitIndex < len("KMGTPE")-1 {
		divisor *= unit
		unitIndex++
	}
	return fmt.Sprintf("%.2f %ciB", float64(value)/float64(divisor), "KMGTPE"[unitIndex])
}

func selectDrives(snapshot domain.Snapshot, list string) ([]domain.Drive, error) {
	parts := strings.Split(list, ",")
	selected := make([]domain.Drive, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		q := strings.TrimSpace(part)
		if q == "" {
			return nil, errors.New("empty drive selector")
		}
		drive, ok := inventory.FindDrive(snapshot, q)
		if !ok {
			return nil, fmt.Errorf("drive %q did not resolve uniquely; present: %s", q, strings.Join(inventory.DriveIDs(snapshot), ", "))
		}
		if seen[drive.ID] {
			return nil, fmt.Errorf("drive %q selected more than once", q)
		}
		seen[drive.ID] = true
		selected = append(selected, drive)
	}
	return selected, nil
}

func propertyMap(values []string) (map[string]string, error) {
	m := map[string]string{}
	for _, item := range values {
		k, v, ok := strings.Cut(item, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("property %q must be key=value", item)
		}
		if _, exists := m[k]; exists {
			return nil, fmt.Errorf("property %q supplied more than once", k)
		}
		m[k] = strings.TrimSpace(v)
	}
	return m, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
