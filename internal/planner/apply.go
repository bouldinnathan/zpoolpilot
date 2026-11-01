// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/terminal"
)

const (
	DefaultLockPath       = "/run/zpoolpilot/apply.lock"
	DefaultCommandTimeout = 10 * time.Minute
	defaultAuditTimeout   = 30 * time.Second
)

// Apply revalidates and executes an immutable, allowlisted plan. The runner is
// never called until TTY, lock, freshness, blocker, audit, and confirmation
// checks have all succeeded.
func Apply(ctx context.Context, plan domain.ActionPlan, options ApplyOptions) (result ApplyResult, returnErr error) {
	if ctx == nil {
		return result, fmt.Errorf("%w: nil context", ErrInvalidPlan)
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	if err := ValidatePlan(plan, now().UTC()); err != nil {
		return result, err
	}
	if len(plan.Blockers) > 0 {
		return result, fmt.Errorf("%w: %s", ErrPlanBlocked, strings.Join(plan.Blockers, "; "))
	}
	if options.Runner == nil {
		return result, fmt.Errorf("%w: runner is required", ErrInvalidPlan)
	}
	if options.FreshSnapshot == nil {
		return result, fmt.Errorf("%w: snapshot callback is required", ErrRevalidation)
	}
	if options.BeforeAudit == nil || options.AfterAudit == nil {
		return result, fmt.Errorf("%w: before and after audit callbacks are required", ErrAudit)
	}
	if plan.Risk == domain.RiskDestructive && options.CommandGuard == nil {
		return result, fmt.Errorf("%w: destructive operation %q requires a live disk-path command guard", ErrRevalidation, plan.Operation)
	}
	isTTY := defaultIsTTY
	if options.IsTTY != nil {
		isTTY = options.IsTTY
	}
	if !isTTY() {
		return result, ErrTTYRequired
	}

	locker := options.Locker
	if locker == nil {
		path := options.LockPath
		if path == "" {
			path = DefaultLockPath
		}
		locker = FileLocker{Path: path}
	}
	lock, err := locker.Lock(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("release apply lock: %w", unlockErr))
		}
	}()

	// Expiry is checked again after lock acquisition; waiting for another
	// operation must never extend a plan's authority.
	if !now().UTC().Before(plan.ExpiresAt) {
		return result, fmt.Errorf("%w: expired while waiting for apply lock", ErrPlanExpired)
	}
	fresh, err := options.FreshSnapshot(ctx)
	if err != nil {
		return result, fmt.Errorf("%w: collect before state: %w", ErrRevalidation, err)
	}
	result.Before = fresh
	if err := revalidate(plan, fresh); err != nil {
		return result, err
	}

	rebuilt, err := RebuildCommands(plan)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(plan.Commands, rebuilt) {
		return result, fmt.Errorf("%w: command preview differs from reconstruction", ErrPlanChanged)
	}
	fingerprint := Fingerprint(plan)
	result.Fingerprint = fingerprint
	beforeRecord := AuditRecord{
		Phase: "before", PlanID: plan.ID, Operation: Operation(plan.Operation),
		Fingerprint: fingerprint, At: now().UTC(), Before: &fresh,
	}
	if err := options.BeforeAudit(ctx, beforeRecord); err != nil {
		return result, fmt.Errorf("%w: record before state: %w", ErrAudit, err)
	}

	commandTimeout := options.CommandTimeout
	if commandTimeout <= 0 {
		commandTimeout = DefaultCommandTimeout
	}
	var operationErr error
	for index, planned := range rebuilt {
		if !now().UTC().Before(plan.ExpiresAt) {
			operationErr = fmt.Errorf("%w: expired before command %d", ErrPlanExpired, index+1)
			break
		}
		request := ConfirmationRequest{
			PlanID: plan.ID, Fingerprint: fingerprint, Command: planned,
			Index: index + 1, Total: len(rebuilt),
		}
		if err := confirmCommand(ctx, options.Confirm, plan, request); err != nil {
			operationErr = err
			break
		}
		immediate, freshErr := options.FreshSnapshot(ctx)
		if freshErr != nil {
			operationErr = fmt.Errorf("%w: collect immediately before command %d: %w", ErrRevalidation, index+1, freshErr)
			break
		}
		if index == 0 {
			freshErr = revalidate(plan, immediate)
		} else {
			freshErr = revalidateContinuation(plan, immediate)
		}
		if freshErr != nil {
			operationErr = fmt.Errorf("before command %d: %w", index+1, freshErr)
			break
		}
		if options.CommandGuard != nil {
			if guardErr := options.CommandGuard(ctx, plan, planned, index+1); guardErr != nil {
				operationErr = fmt.Errorf("%w: command %d live guard: %w", ErrRevalidation, index+1, guardErr)
				break
			}
		}
		if !now().UTC().Before(plan.ExpiresAt) {
			operationErr = fmt.Errorf("%w: expired immediately before command %d", ErrPlanExpired, index+1)
			break
		}
		capability := execx.CapabilityMutate
		if planned.Risk == domain.RiskRead {
			capability = execx.CapabilityObserve
		}
		execution, runErr := options.Runner.Run(ctx, execx.Command{
			Program: planned.Program, Args: append([]string(nil), planned.Args...),
			Capability: capability, Risk: planned.Risk, Timeout: commandTimeout,
			Interactive: commandNeedsTerminal(plan, planned),
		})
		if execution.Trace.Program != "" {
			result.Commands = append(result.Commands, execution.Trace)
		}
		if runErr != nil {
			operationErr = fmt.Errorf("run command %d (%s): %w", index+1, planned.Program, runErr)
			break
		}
	}

	after, snapshotErr := options.FreshSnapshot(ctx)
	if snapshotErr == nil {
		result.After = after
	} else {
		operationErr = errors.Join(operationErr, fmt.Errorf("%w: collect after state: %w", ErrRevalidation, snapshotErr))
	}
	afterRecord := AuditRecord{
		Phase: "after", PlanID: plan.ID, Operation: Operation(plan.Operation),
		Fingerprint: fingerprint, At: now().UTC(), Before: &fresh,
		Commands: append([]domain.CommandTrace(nil), result.Commands...),
	}
	if snapshotErr == nil {
		afterRecord.After = &after
	}
	if operationErr != nil {
		afterRecord.Error = operationErr.Error()
	}
	auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), defaultAuditTimeout)
	defer cancelAudit()
	if auditErr := options.AfterAudit(auditCtx, afterRecord); auditErr != nil {
		operationErr = errors.Join(operationErr, fmt.Errorf("%w: record after state: %w", ErrAudit, auditErr))
	}
	if operationErr != nil {
		return result, operationErr
	}
	return result, nil
}

func commandNeedsTerminal(plan domain.ActionPlan, command domain.PlannedCommand) bool {
	if Operation(plan.Operation) == OperationReplaceBootDisk && command.Program == "proxmox-boot-tool" &&
		len(command.Args) > 0 && (command.Args[0] == "format" || command.Args[0] == "init") {
		return true
	}
	if Operation(plan.Operation) != OperationCreatePool || command.Program != "zpool" {
		return false
	}
	properties, err := desiredProperties(plan.Desired["dataset_properties"])
	if err != nil {
		return false
	}
	return properties["encryption"] != "" && properties["encryption"] != "off" && properties["keylocation"] == "prompt"
}

func confirmCommand(ctx context.Context, callbacks Confirmations, plan domain.ActionPlan, request ConfirmationRequest) error {
	switch request.Command.Risk {
	case domain.RiskRead:
		if callbacks.Green == nil {
			return nil
		}
		_, err := callbacks.Green(ctx, request)
		return err
	case domain.RiskChange:
		if callbacks.Yellow == nil {
			return fmt.Errorf("%w: yellow confirmation callback is required", ErrConfirmation)
		}
		request.Expected = "[Y/n]"
		response, err := callbacks.Yellow(ctx, request)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrConfirmation, err)
		}
		switch strings.ToLower(strings.TrimSpace(response)) {
		case "", "y", "yes":
			return nil
		default:
			return ErrConfirmation
		}
	case domain.RiskDestructive:
		if callbacks.Red == nil {
			return fmt.Errorf("%w: red confirmation callback is required", ErrConfirmation)
		}
		request.Expected = plan.Confirmation
		response, err := callbacks.Red(ctx, request)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrConfirmation, err)
		}
		if response != request.Expected {
			return fmt.Errorf("%w: expected exact text %q", ErrConfirmation, request.Expected)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown command risk %q", ErrInvalidPlan, request.Command.Risk)
	}
}

func revalidate(plan domain.ActionPlan, snapshot domain.Snapshot) error {
	if snapshot.Host.MachineID != plan.HostMachineID {
		return fmt.Errorf("%w: host machine id changed from %q to %q", ErrRevalidation, plan.HostMachineID, snapshot.Host.MachineID)
	}
	if reasons := freshSafetyBlockers(plan, snapshot); len(reasons) > 0 {
		return fmt.Errorf("%w: %s", ErrRevalidation, strings.Join(sortedUnique(reasons), "; "))
	}
	if TopologyFingerprint(snapshot) != plan.TopologyFingerprint {
		return fmt.Errorf("%w: pool topology fingerprint changed", ErrPlanChanged)
	}
	if SnapshotFingerprint(snapshot) != plan.SnapshotFingerprint {
		return fmt.Errorf("%w: safety-relevant snapshot fingerprint changed", ErrPlanChanged)
	}
	return nil
}

func revalidateContinuation(plan domain.ActionPlan, snapshot domain.Snapshot) error {
	if snapshot.Host.MachineID != plan.HostMachineID {
		return fmt.Errorf("%w: host machine id changed from %q to %q", ErrRevalidation, plan.HostMachineID, snapshot.Host.MachineID)
	}
	reasons := freshTargetBlockers(plan, snapshot)
	operation := Operation(plan.Operation)
	if plan.Risk == domain.RiskDestructive && operation != OperationCreatePool && operation != OperationPrepareDisk {
		if pool, found := findPoolRef(snapshot, plan.TargetPool); found {
			reasons = append(reasons, poolActivityBlockers(pool)...)
		} else {
			reasons = append(reasons, fmt.Sprintf("target pool %q with GUID %q is missing", plan.TargetPool.Name, plan.TargetPool.GUID))
		}
	}
	if operation == OperationReplaceBootDisk {
		reasons = append(reasons, freshBootBlockers(plan, snapshot, true)...)
	}
	if len(reasons) > 0 {
		return fmt.Errorf("%w: %s", ErrRevalidation, strings.Join(sortedUnique(reasons), "; "))
	}
	if TopologyFingerprint(snapshot) != plan.TopologyFingerprint {
		return fmt.Errorf("%w: pool topology fingerprint changed", ErrPlanChanged)
	}
	return nil
}

func freshSafetyBlockers(plan domain.ActionPlan, snapshot domain.Snapshot) []string {
	blockers := freshTargetBlockers(plan, snapshot)

	operation := Operation(plan.Operation)
	if operation == OperationCreatePool {
		for _, pool := range snapshot.Pools {
			if pool.Name == plan.TargetPool.Name {
				blockers = append(blockers, fmt.Sprintf("pool %q now exists", pool.Name))
			}
			blockers = append(blockers, poolActivityBlockers(pool)...)
		}
		topology, err := desiredTopology(plan)
		if err != nil {
			blockers = append(blockers, err.Error())
			return blockers
		}
		expert, err := desiredBool(plan, "expert")
		if err != nil {
			blockers = append(blockers, err.Error())
			return blockers
		}
		temporary := domain.ActionPlan{}
		validateTopology(&temporary, topology, len(plan.TargetDrives), expert)
		capacities := make([]uint64, 0, len(plan.TargetDrives))
		for _, drive := range plan.TargetDrives {
			capacities = append(capacities, drive.CapacityBytes)
		}
		validateMixedCapacities(&temporary, capacities, expert)
		poolProperties, poolErr := desiredProperties(plan.Desired["pool_properties"])
		datasetProperties, datasetErr := desiredProperties(plan.Desired["dataset_properties"])
		if poolErr != nil || datasetErr != nil {
			blockers = append(blockers, "create properties can no longer be safely decoded")
		} else {
			validateProperties(&temporary, poolProperties, datasetProperties, expert)
		}
		blockers = append(blockers, temporary.Blockers...)
		return blockers
	}
	if operation == OperationPrepareDisk {
		if len(plan.TargetDrives) == 1 {
			for _, drive := range snapshot.Drives {
				if drive.ID != plan.TargetDrives[0].ID {
					continue
				}
				for _, pool := range snapshot.Pools {
					if driveIsPoolMember(pool, drive.ID) || driveUseNamesPool(drive.Use, pool.Name) {
						for _, blocker := range poolActivityBlockers(pool) {
							blockers = append(blockers, fmt.Sprintf("target drive %q belongs to %s", drive.ID, blocker))
						}
					}
				}
			}
		}
		return blockers
	}

	pool, found := findPoolRef(snapshot, plan.TargetPool)
	if !found {
		blockers = append(blockers, fmt.Sprintf("target pool %q with GUID %q is missing", plan.TargetPool.Name, plan.TargetPool.GUID))
		return blockers
	}
	if pool.BootCritical && operation != OperationReplaceBootDisk && !maintenanceOperation(operation) {
		blockers = append(blockers, fmt.Sprintf("pool %q is boot-critical; generic apply cannot mutate it", pool.Name))
	}
	if plan.Risk == domain.RiskDestructive {
		blockers = append(blockers, poolActivityBlockers(pool)...)
	}

	switch operation {
	case OperationReplaceDisk, OperationAttachMirror, OperationExpandRAIDZ:
		vdev, parent, found := lookupVDev(pool, plan.TargetVDev.GUID)
		if !found {
			blockers = append(blockers, fmt.Sprintf("target vdev GUID %q is missing", plan.TargetVDev.GUID))
			break
		}
		if len(plan.TargetDrives) == 1 {
			target := plan.TargetDrives[0]
			if vdevContainsDriveID(vdev, target.ID) {
				switch operation {
				case OperationReplaceDisk:
					blockers = append(blockers, "replacement target is the same stable device as the old vdev")
				case OperationAttachMirror:
					blockers = append(blockers, "mirror target is the same stable device as the existing vdev")
				case OperationExpandRAIDZ:
					blockers = append(blockers, "expansion target is already a member of the selected RAIDZ vdev")
				}
			}
			capacity := target.CapacityBytes
			required := vdev.SizeBytes
			if operation == OperationExpandRAIDZ {
				required = minimumChildSize(vdev)
				if required == 0 {
					blockers = append(blockers, fmt.Sprintf("target RAIDZ vdev %q no longer has complete child-size authority", vdev.Name))
				}
				if !strings.HasPrefix(string(vdevTopology(vdev)), "raidz") || !isTopLevelVDev(pool, vdev.GUID) {
					blockers = append(blockers, fmt.Sprintf("target vdev %q is no longer a top-level RAIDZ vdev", vdev.Name))
				}
			} else if required == 0 {
				blockers = append(blockers, fmt.Sprintf("target vdev %q no longer has size authority", vdev.Name))
			}
			if required > 0 && capacity < required {
				blockers = append(blockers, fmt.Sprintf("target drive is too small: %d bytes available, %d required", capacity, required))
			}
			if operation == OperationAttachMirror && (strings.HasPrefix(parent, "raidz") || strings.HasPrefix(parent, "draid") || len(vdev.Children) != 0) {
				blockers = append(blockers, "target is no longer an attachable leaf vdev")
			}
		}
		temporary := domain.ActionPlan{}
		switch operation {
		case OperationReplaceDisk:
			validateReplacementRedundancy(&temporary, pool, vdev)
		case OperationAttachMirror:
			if !online(pool.State) || !online(vdev.State) {
				temporary.Blockers = append(temporary.Blockers, "pool and attach target must remain ONLINE")
			}
		case OperationExpandRAIDZ:
			if !openZFSAtLeast23(snapshot.Host.OpenZFSVersion) {
				temporary.Blockers = append(temporary.Blockers, "OpenZFS version no longer satisfies the 2.3+ RAIDZ expansion requirement")
			}
			if !online(pool.State) || !allVDevsOnline(vdev) {
				temporary.Blockers = append(temporary.Blockers, "pool and every RAIDZ child must remain ONLINE")
			}
		}
		blockers = append(blockers, temporary.Blockers...)
	case OperationAddVDev:
		topology, err := desiredTopology(plan)
		if err != nil {
			blockers = append(blockers, err.Error())
			break
		}
		temporary := domain.ActionPlan{}
		validateTopology(&temporary, topology, len(plan.TargetDrives), false)
		capacities := make([]uint64, 0, len(plan.TargetDrives))
		for _, drive := range plan.TargetDrives {
			capacities = append(capacities, drive.CapacityBytes)
		}
		validateMixedCapacities(&temporary, capacities, false)
		validateMatchingVDev(&temporary, pool, topology, len(plan.TargetDrives))
		if !online(pool.State) || !allDataVDevsOnline(pool) {
			temporary.Blockers = append(temporary.Blockers, "pool and all data vdevs must remain ONLINE")
		}
		blockers = append(blockers, temporary.Blockers...)
	case OperationAddSpare:
		required, complete := maximumDataLeafSize(pool)
		if !complete {
			blockers = append(blockers, fmt.Sprintf("pool %q no longer has complete data-leaf size authority", pool.Name))
		} else {
			for _, target := range plan.TargetDrives {
				if target.CapacityBytes < required {
					blockers = append(blockers, fmt.Sprintf("target spare %q is too small for the pool's largest data leaf: %d bytes available, %d required", target.ID, target.CapacityBytes, required))
				}
			}
		}
	case OperationReplaceBootDisk:
		blockers = append(blockers, freshBootBlockers(plan, snapshot, false)...)
	case OperationScrubStart, OperationScrubStop, OperationTrimStart, OperationTrimStop, OperationClearErrors:
		blockers = append(blockers, maintenancePlanBlockers(plan, pool)...)
	case OperationPrepareDisk:
		// Common target-drive validation is sufficient.
	}
	return blockers
}

func freshTargetBlockers(plan domain.ActionPlan, snapshot domain.Snapshot) []string {
	var blockers []string
	for _, ref := range plan.TargetDrives {
		if !validSectorGeometry(ref.LogicalSectorBytes, ref.PhysicalSectorBytes) {
			blockers = append(blockers, fmt.Sprintf("target drive %q has invalid planned logical/physical sector geometry (%d/%d)", ref.ID, ref.LogicalSectorBytes, ref.PhysicalSectorBytes))
		}
		matches := make([]domain.Drive, 0, 1)
		for _, drive := range snapshot.Drives {
			if drive.ID == ref.ID {
				matches = append(matches, drive)
			}
		}
		if len(matches) != 1 {
			blockers = append(blockers, fmt.Sprintf("target drive %q resolved to %d devices", ref.ID, len(matches)))
			continue
		}
		drive := matches[0]
		// Repeat this check for every prepare step. Removing a partition table
		// does not authorize wiping a raw ownership signature that appears in
		// the next discovery, including one previously hidden by that table.
		if Operation(plan.Operation) == OperationPrepareDisk && unsafeRawOwnershipSignature(drive.Use) {
			blockers = append(blockers, fmt.Sprintf("target drive %q contains a raw storage ownership signature that cannot be proven inactive (%s)", drive.ID, drive.Use))
		}
		if drive.WWN != ref.WWN || drive.Serial != ref.Serial || drive.ByID != ref.ByID || drive.CapacityBytes != ref.CapacityBytes ||
			drive.LogicalSectorBytes != ref.LogicalSectorBytes || drive.PhysicalSectorBytes != ref.PhysicalSectorBytes || driveMajorMinor(drive) != ref.MajorMinor {
			blockers = append(blockers, fmt.Sprintf("target drive %q identity, capacity, or sector geometry changed", ref.ID))
		}
		if !validSectorGeometry(drive.LogicalSectorBytes, drive.PhysicalSectorBytes) {
			blockers = append(blockers, fmt.Sprintf("target drive %q no longer has valid logical/physical sector geometry (%d/%d)", ref.ID, drive.LogicalSectorBytes, drive.PhysicalSectorBytes))
		}
		if Operation(plan.Operation) == OperationPrepareDisk || Operation(plan.Operation) == OperationReplaceBootDisk {
			if drive.InUse && activeUseForPrepare(drive.Use) {
				blockers = append(blockers, fmt.Sprintf("target drive %q became actively used (%s)", ref.ID, drive.Use))
			}
		} else if drive.InUse || protectedUse(drive.Use) {
			blockers = append(blockers, fmt.Sprintf("target drive %q became active (%s)", ref.ID, drive.Use))
		}
		if !validByIDPath(drive.ByID) || !reliableDriveIdentity(drive) {
			blockers = append(blockers, fmt.Sprintf("target drive %q no longer has a safe stable identity", ref.ID))
		}
		if drive.WWN == "" && serialCount(snapshot, drive.Serial) > 1 {
			blockers = append(blockers, fmt.Sprintf("target drive %q serial is ambiguous", ref.ID))
		}
		if drive.Bay.Label != ref.Bay {
			blockers = append(blockers, fmt.Sprintf("target drive %q physical bay mapping changed from %q to %q", ref.ID, ref.Bay, drive.Bay.Label))
		}
		if drive.Bay.Confidence == "conflict" {
			blockers = append(blockers, fmt.Sprintf("target drive %q now has a conflicting physical bay mapping", ref.ID))
		}
		if operationRequiresHealthyTarget(Operation(plan.Operation)) {
			blockers = append(blockers, targetDriveHealthBlockers(snapshot, drive)...)
		}
	}
	return blockers
}

func freshBootBlockers(plan domain.ActionPlan, snapshot domain.Snapshot, allowExpectedTargetSignatures bool) []string {
	var blockers []string
	if !strings.HasPrefix(strings.ToLower(snapshot.Host.ProxmoxVersion), "pve-manager/") || snapshot.Host.ProxmoxNode == "" {
		blockers = append(blockers, "Proxmox VE host evidence is no longer present")
	}
	mode, modeErr := desiredBootMode(plan)
	if modeErr != nil {
		blockers = append(blockers, modeErr.Error())
	} else if snapshot.Host.ProxmoxBootMode == "" {
		blockers = append(blockers, "Proxmox boot mode is no longer consistently discoverable")
	} else if snapshot.Host.ProxmoxBootMode != string(mode) {
		blockers = append(blockers, fmt.Sprintf("Proxmox boot mode changed to %q from planned %q", snapshot.Host.ProxmoxBootMode, mode))
	}
	if len(plan.SourceDrives) != 1 {
		return append(blockers, "boot replacement no longer has exactly one source drive reference")
	}
	if !allowExpectedTargetSignatures && len(plan.TargetDrives) == 1 {
		for _, drive := range snapshot.Drives {
			if drive.ID == plan.TargetDrives[0].ID && drive.InUse {
				blockers = append(blockers, fmt.Sprintf("replacement boot drive %q is not clean/prepared immediately before execution (%s)", drive.ID, drive.Use))
			}
		}
	}
	sourceRef := plan.SourceDrives[0]
	var sources []domain.Drive
	for _, drive := range snapshot.Drives {
		if drive.ID == sourceRef.ID {
			sources = append(sources, drive)
		}
	}
	if len(sources) != 1 {
		return append(blockers, fmt.Sprintf("boot source drive %q resolved to %d devices", sourceRef.ID, len(sources)))
	}
	source := sources[0]
	if !validSectorGeometry(sourceRef.LogicalSectorBytes, sourceRef.PhysicalSectorBytes) {
		blockers = append(blockers, fmt.Sprintf("boot source drive %q has invalid planned logical/physical sector geometry (%d/%d)", sourceRef.ID, sourceRef.LogicalSectorBytes, sourceRef.PhysicalSectorBytes))
	}
	if source.WWN != sourceRef.WWN || source.Serial != sourceRef.Serial || source.ByID != sourceRef.ByID ||
		source.CapacityBytes != sourceRef.CapacityBytes || source.LogicalSectorBytes != sourceRef.LogicalSectorBytes ||
		source.PhysicalSectorBytes != sourceRef.PhysicalSectorBytes || driveMajorMinor(source) != sourceRef.MajorMinor || !wholeDiskByID(source.ByID) {
		blockers = append(blockers, fmt.Sprintf("boot source drive %q identity, capacity, sector geometry, or whole-disk path changed", sourceRef.ID))
	}
	if !validSectorGeometry(source.LogicalSectorBytes, source.PhysicalSectorBytes) {
		blockers = append(blockers, fmt.Sprintf("boot source drive %q no longer has valid logical/physical sector geometry (%d/%d)", sourceRef.ID, source.LogicalSectorBytes, source.PhysicalSectorBytes))
	}
	pool, found := findPoolRef(snapshot, plan.TargetPool)
	if !found {
		return append(blockers, fmt.Sprintf("boot pool %q with GUID %q is missing", plan.TargetPool.Name, plan.TargetPool.GUID))
	}
	if !pool.BootCritical {
		blockers = append(blockers, fmt.Sprintf("pool %q is no longer marked boot-critical", pool.Name))
	}
	if !online(pool.State) && !strings.EqualFold(pool.State, "DEGRADED") {
		blockers = append(blockers, fmt.Sprintf("boot pool %q state changed to %q", pool.Name, pool.State))
	}
	if !driveUseNamesPool(source.Use, pool.Name) {
		blockers = append(blockers, fmt.Sprintf("source drive %q is no longer confirmed in boot pool %q", source.ID, pool.Name))
	}
	if !source.InUse {
		blockers = append(blockers, fmt.Sprintf("source drive %q is no longer marked as an active device", source.ID))
	}
	if !healthyBootSource(source) {
		blockers = append(blockers, fmt.Sprintf("source drive %q no longer has a healthy drive-health assessment", source.ID))
	}
	oldVDev, _, found := lookupVDev(pool, plan.TargetVDev.GUID)
	if !found {
		return append(blockers, fmt.Sprintf("old boot vdev GUID %q is missing before final replacement", plan.TargetVDev.GUID))
	}
	if len(oldVDev.Children) != 0 {
		blockers = append(blockers, fmt.Sprintf("old boot vdev %q is no longer a leaf", oldVDev.Name))
	}
	if len(plan.TargetDrives) == 1 && vdevContainsDriveID(oldVDev, plan.TargetDrives[0].ID) {
		blockers = append(blockers, "replacement target is the same stable device as the old boot vdev")
	}
	temporary := domain.ActionPlan{}
	validateBootSibling(&temporary, pool, oldVDev, source)
	blockers = append(blockers, temporary.Blockers...)
	if len(plan.TargetDrives) == 1 && plan.TargetDrives[0].CapacityBytes < source.CapacityBytes {
		blockers = append(blockers, "replacement boot drive is now smaller than the healthy partition-table source")
	}
	if len(plan.TargetDrives) == 1 && plan.TargetDrives[0].LogicalSectorBytes != source.LogicalSectorBytes {
		blockers = append(blockers, "boot partition-table source and target logical sector sizes must match")
	}
	if len(plan.TargetDrives) == 1 && oldVDev.SizeBytes > 0 && plan.TargetDrives[0].CapacityBytes < oldVDev.SizeBytes {
		blockers = append(blockers, "replacement boot drive is now smaller than the old boot-pool vdev")
	}
	if len(plan.TargetDrives) == 1 {
		for _, drive := range snapshot.Drives {
			if drive.ID == plan.TargetDrives[0].ID && !acceptableBootTarget(drive) {
				blockers = append(blockers, fmt.Sprintf("replacement drive %q now reports failed or critical hardware health", drive.ID))
			}
		}
	}
	return blockers
}

func desiredBool(plan domain.ActionPlan, key string) (bool, error) {
	value, ok := plan.Desired[key].(bool)
	if !ok {
		return false, fmt.Errorf("%w: desired field %q must be boolean", ErrInvalidPlan, key)
	}
	return value, nil
}

func findPoolRef(snapshot domain.Snapshot, ref domain.PoolRef) (domain.Pool, bool) {
	for _, pool := range snapshot.Pools {
		if pool.GUID == ref.GUID && pool.Name == ref.Name {
			return pool, true
		}
	}
	return domain.Pool{}, false
}

func lookupVDev(pool domain.Pool, guid string) (domain.VDev, string, bool) {
	for _, root := range pool.VDevs {
		if result, parent, found := findVDev(root, guid, ""); found {
			return result, parent, true
		}
	}
	return domain.VDev{}, "", false
}

func defaultIsTTY() bool {
	return terminal.IsTerminal(os.Stdin)
}

// FileLocker provides a process-wide advisory apply lock.
type FileLocker struct {
	Path string
}

func (locker FileLocker) Lock(ctx context.Context) (Unlocker, error) {
	if locker.Path == "" || locker.Path[0] != '/' {
		return nil, fmt.Errorf("%w: apply lock path must be absolute", ErrInvalidPlan)
	}
	if err := os.MkdirAll(filepathDir(locker.Path), 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	fd, err := syscall.Open(locker.Path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open apply lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), locker.Path)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileUnlocker{file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = file.Close()
			return nil, fmt.Errorf("acquire apply lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("%w: %w", ErrLockHeld, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// filepathDir is local to keep apply's filesystem surface obvious.
func filepathDir(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return "."
	}
	if index == 0 {
		return "/"
	}
	return path[:index]
}

type fileUnlocker struct {
	file *os.File
}

func (unlocker *fileUnlocker) Unlock() error {
	if unlocker == nil || unlocker.file == nil {
		return nil
	}
	fd := int(unlocker.file.Fd())
	err := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := unlocker.file.Close()
	unlocker.file = nil
	return errors.Join(err, closeErr)
}
