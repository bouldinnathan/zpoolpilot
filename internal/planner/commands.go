// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// RebuildCommands creates the only executable argv permitted for a plan. It
// ignores plan.Commands entirely, so callers can compare serialized previews
// against this trusted reconstruction before apply.
func RebuildCommands(plan domain.ActionPlan) ([]domain.PlannedCommand, error) {
	operation := Operation(plan.Operation)
	if !knownOperation(operation) {
		return nil, fmt.Errorf("%w: unknown operation %q", ErrInvalidPlan, plan.Operation)
	}
	if err := validateDesiredKeys(plan, operation); err != nil {
		return nil, err
	}
	if err := validateDriveRefs(plan.SourceDrives); err != nil {
		return nil, err
	}
	command := func(program string, args []string, reason string) domain.PlannedCommand {
		return domain.PlannedCommand{Program: program, Args: args, Risk: operationRisk(operation), Reason: reason}
	}

	switch operation {
	case OperationCreatePool:
		if !validPoolName(plan.TargetPool.Name) || plan.TargetPool.GUID != "" {
			return nil, fmt.Errorf("%w: invalid create-pool target", ErrInvalidPlan)
		}
		if _, err := desiredBool(plan, "expert"); err != nil {
			return nil, err
		}
		topology, err := desiredTopology(plan)
		if err != nil {
			return nil, err
		}
		poolProperties, err := desiredProperties(plan.Desired["pool_properties"])
		if err != nil {
			return nil, fmt.Errorf("%w: pool properties: %v", ErrInvalidPlan, err)
		}
		datasetProperties, err := desiredProperties(plan.Desired["dataset_properties"])
		if err != nil {
			return nil, fmt.Errorf("%w: dataset properties: %v", ErrInvalidPlan, err)
		}
		for key, value := range poolProperties {
			if !validPoolProperty(key, value) {
				return nil, fmt.Errorf("%w: unapproved pool property %s=%q", ErrInvalidPlan, key, value)
			}
		}
		for key, value := range datasetProperties {
			if !validDatasetProperty(key, value) {
				return nil, fmt.Errorf("%w: unapproved dataset property %s=%q", ErrInvalidPlan, key, value)
			}
		}
		if err := validateEncryptionProperties(datasetProperties); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidPlan, err)
		}
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil {
			return nil, err
		}
		args := []string{"create"}
		args = appendProperties(args, "-o", poolProperties)
		args = appendProperties(args, "-O", datasetProperties)
		args = append(args, plan.TargetPool.Name)
		if topology != TopologySingle {
			args = append(args, string(topology))
		}
		args = append(args, paths...)
		return []domain.PlannedCommand{command("zpool", args, "create the planned pool using persistent drive paths")}, nil

	case OperationPrepareDisk:
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil || len(paths) != 1 {
			return nil, fmt.Errorf("%w: prepare-disk requires exactly one target drive", ErrInvalidPlan)
		}
		return []domain.PlannedCommand{
			command("sgdisk", []string{"--zap-all", paths[0]}, "remove primary and backup partition tables"),
			command("wipefs", []string{"--all", paths[0]}, "remove recognized filesystem and RAID signatures"),
		}, nil

	case OperationReplaceDisk:
		if err := validateExistingTarget(plan, true); err != nil {
			return nil, err
		}
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil || len(paths) != 1 {
			return nil, fmt.Errorf("%w: replace-disk requires exactly one target drive", ErrInvalidPlan)
		}
		return []domain.PlannedCommand{command("zpool", []string{"replace", plan.TargetPool.Name, plan.TargetVDev.GUID, paths[0]}, "replace the selected leaf vdev")}, nil

	case OperationAttachMirror:
		if err := validateExistingTarget(plan, true); err != nil {
			return nil, err
		}
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil || len(paths) != 1 {
			return nil, fmt.Errorf("%w: attach-mirror requires exactly one target drive", ErrInvalidPlan)
		}
		return []domain.PlannedCommand{command("zpool", []string{"attach", plan.TargetPool.Name, plan.TargetVDev.GUID, paths[0]}, "attach a new leaf and form or widen a mirror")}, nil

	case OperationExpandRAIDZ:
		if err := validateExistingTarget(plan, true); err != nil {
			return nil, err
		}
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil || len(paths) != 1 {
			return nil, fmt.Errorf("%w: expand-raidz requires exactly one target drive", ErrInvalidPlan)
		}
		return []domain.PlannedCommand{command("zpool", []string{"attach", plan.TargetPool.Name, plan.TargetVDev.GUID, paths[0]}, "expand the selected RAIDZ vdev by one device")}, nil

	case OperationAddVDev:
		if err := validateExistingTarget(plan, false); err != nil {
			return nil, err
		}
		topology, err := desiredTopology(plan)
		if err != nil {
			return nil, err
		}
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil {
			return nil, err
		}
		args := []string{"add", plan.TargetPool.Name}
		if topology != TopologySingle {
			args = append(args, string(topology))
		}
		args = append(args, paths...)
		return []domain.PlannedCommand{command("zpool", args, "add one matching top-level data vdev")}, nil

	case OperationAddSpare:
		if err := validateExistingTarget(plan, false); err != nil {
			return nil, err
		}
		paths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil {
			return nil, err
		}
		args := append([]string{"add", plan.TargetPool.Name, "spare"}, paths...)
		return []domain.PlannedCommand{command("zpool", args, "add selected drives as hot spares")}, nil

	case OperationReplaceBootDisk:
		if err := validateExistingTarget(plan, true); err != nil {
			return nil, err
		}
		mode, err := desiredBootMode(plan)
		if err != nil {
			return nil, err
		}
		if len(plan.SourceDrives) != 1 || len(plan.TargetDrives) != 1 {
			return nil, fmt.Errorf("%w: boot replacement requires exactly one source and one target whole disk", ErrInvalidPlan)
		}
		sourcePaths, err := drivePaths(plan.SourceDrives, 1)
		if err != nil || len(sourcePaths) != 1 || !wholeDiskByID(sourcePaths[0]) {
			return nil, fmt.Errorf("%w: unsafe boot source whole-disk reference", ErrInvalidPlan)
		}
		targetPaths, err := drivePaths(plan.TargetDrives, 1)
		if err != nil || len(targetPaths) != 1 || !wholeDiskByID(targetPaths[0]) {
			return nil, fmt.Errorf("%w: unsafe boot target whole-disk reference", ErrInvalidPlan)
		}
		if sourcePaths[0] == targetPaths[0] || plan.SourceDrives[0].ID == plan.TargetDrives[0].ID {
			return nil, fmt.Errorf("%w: boot source and target must be different stable disks", ErrInvalidPlan)
		}
		esp, err := partitionByID(targetPaths[0], ProxmoxESPPartition)
		if err != nil {
			return nil, err
		}
		zfsPartition, err := partitionByID(targetPaths[0], ProxmoxZFSPartition)
		if err != nil {
			return nil, err
		}
		commands := []domain.PlannedCommand{
			command("sgdisk", []string{sourcePaths[0], "-R", targetPaths[0]}, "clone the healthy Proxmox boot disk partition table onto the replacement whole disk"),
			command("sgdisk", []string{"-G", targetPaths[0]}, "randomize the replacement disk and partition GUIDs"),
			command("partprobe", []string{targetPaths[0]}, "ask the kernel to read the replacement partition table"),
			command("proxmox-boot-tool", []string{"format", esp}, "format the fixed Proxmox installer ESP partition 2"),
		}
		initArgs := []string{"init", esp}
		if mode == BootModeGRUB {
			initArgs = append(initArgs, "grub")
		}
		commands = append(commands,
			command("proxmox-boot-tool", initArgs, "initialize the replacement ESP in the explicitly selected boot mode"),
			command("zpool", []string{"replace", plan.TargetPool.Name, plan.TargetVDev.GUID, zfsPartition}, "replace the old boot-pool leaf using fixed Proxmox installer ZFS partition 3"),
		)
		return commands, nil

	case OperationScrubStart, OperationScrubStop, OperationTrimStart, OperationTrimStop, OperationClearErrors:
		if err := validateExistingTarget(plan, false); err != nil {
			return nil, err
		}
		if operation == OperationTrimStart {
			if _, err := desiredTrimIntent(plan); err != nil {
				return nil, err
			}
		}
		if plan.TargetVDev.Name != "" || plan.TargetVDev.GUID != "" || len(plan.SourceDrives) != 0 || len(plan.TargetDrives) != 0 {
			return nil, fmt.Errorf("%w: routine maintenance may target only one pool", ErrInvalidPlan)
		}
		var args []string
		var reason string
		switch operation {
		case OperationScrubStart:
			args = []string{"scrub", plan.TargetPool.Name}
			reason = "start a scrub on the selected pool"
		case OperationScrubStop:
			args = []string{"scrub", "-s", plan.TargetPool.Name}
			reason = "stop only an already-active scrub on the selected pool"
		case OperationTrimStart:
			args = []string{"trim", plan.TargetPool.Name}
			reason = "start manual TRIM on supported devices in the selected pool"
		case OperationTrimStop:
			args = []string{"trim", "-s", plan.TargetPool.Name}
			reason = "suspend only an already-active manual TRIM on the selected pool"
		case OperationClearErrors:
			args = []string{"clear", plan.TargetPool.Name}
			reason = "clear recorded device errors without changing pool topology"
		}
		return []domain.PlannedCommand{command("zpool", args, reason)}, nil
	}
	panic("unreachable")
}

// ValidatePlan validates schema, lifetime, confirmation, and that the stored
// preview is byte-for-byte identical to trusted command reconstruction.
func ValidatePlan(plan domain.ActionPlan, now time.Time) error {
	if plan.APIVersion != domain.PlanAPIVersion || plan.Kind != PlanKind {
		return fmt.Errorf("%w: unsupported schema or kind", ErrInvalidPlan)
	}
	if err := validatePlanID(plan.ID); err != nil {
		return fmt.Errorf("%w: plan id must be 32 lowercase hexadecimal characters", ErrInvalidPlan)
	}
	if !knownOperation(Operation(plan.Operation)) {
		return fmt.Errorf("%w: unknown operation %q", ErrInvalidPlan, plan.Operation)
	}
	expectedRisk := operationRisk(Operation(plan.Operation))
	if plan.Risk != expectedRisk {
		return fmt.Errorf("%w: %s operation must have risk %q", ErrInvalidPlan, plan.Operation, expectedRisk)
	}
	if plan.HostMachineID == "" {
		return fmt.Errorf("%w: host machine id is required", ErrInvalidPlan)
	}
	if plan.CreatedAt.IsZero() || plan.ExpiresAt.IsZero() || !plan.ExpiresAt.After(plan.CreatedAt) || plan.ExpiresAt.Sub(plan.CreatedAt) > MaxPlanTTL {
		return fmt.Errorf("%w: invalid plan lifetime", ErrInvalidPlan)
	}
	if plan.CreatedAt.After(now.Add(time.Minute)) || plan.ExpiresAt.After(now.Add(MaxPlanTTL)) {
		return fmt.Errorf("%w: plan lifetime is unreasonably far in the future", ErrInvalidPlan)
	}
	if !now.Before(plan.ExpiresAt) {
		return fmt.Errorf("%w: expired at %s", ErrPlanExpired, plan.ExpiresAt.Format(time.RFC3339))
	}
	if err := validateSHA256(plan.TopologyFingerprint); err != nil {
		return fmt.Errorf("%w: invalid topology fingerprint", ErrInvalidPlan)
	}
	if err := validateSHA256(plan.SnapshotFingerprint); err != nil {
		return fmt.Errorf("%w: invalid snapshot fingerprint", ErrInvalidPlan)
	}
	rebuilt, err := RebuildCommands(plan)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(plan.Commands, rebuilt) {
		return fmt.Errorf("%w: serialized command preview differs from trusted reconstruction", ErrPlanChanged)
	}
	if expectedRisk == domain.RiskDestructive {
		if plan.Confirmation != expectedRedConfirmation(plan) {
			return fmt.Errorf("%w: confirmation challenge differs from plan fingerprint", ErrPlanChanged)
		}
	} else if plan.Confirmation != "" {
		return fmt.Errorf("%w: yellow plans must not contain a red confirmation challenge", ErrPlanChanged)
	}
	return nil
}

func knownOperation(operation Operation) bool {
	switch operation {
	case OperationCreatePool, OperationPrepareDisk, OperationReplaceDisk, OperationAttachMirror, OperationExpandRAIDZ, OperationAddVDev, OperationAddSpare, OperationReplaceBootDisk,
		OperationScrubStart, OperationScrubStop, OperationTrimStart, OperationTrimStop, OperationClearErrors:
		return true
	default:
		return false
	}
}

func maintenanceOperation(operation Operation) bool {
	switch operation {
	case OperationScrubStart, OperationScrubStop, OperationTrimStart, OperationTrimStop, OperationClearErrors:
		return true
	default:
		return false
	}
}

func operationRisk(operation Operation) domain.Risk {
	if maintenanceOperation(operation) {
		return domain.RiskChange
	}
	return domain.RiskDestructive
}

func validateDesiredKeys(plan domain.ActionPlan, operation Operation) error {
	allowed := map[string]bool{}
	switch operation {
	case OperationCreatePool:
		allowed = map[string]bool{"topology": true, "pool_properties": true, "dataset_properties": true, "expert": true}
	case OperationAddVDev:
		allowed = map[string]bool{"topology": true}
	case OperationReplaceBootDisk:
		allowed = map[string]bool{"boot_mode": true}
	case OperationTrimStart:
		allowed = map[string]bool{"trim_intent": true}
	}
	for key := range plan.Desired {
		if !allowed[key] {
			return fmt.Errorf("%w: desired field %q is not valid for %s", ErrInvalidPlan, key, operation)
		}
	}
	for key := range allowed {
		if _, ok := plan.Desired[key]; !ok {
			return fmt.Errorf("%w: desired field %q is required for %s", ErrInvalidPlan, key, operation)
		}
	}
	return nil
}

func desiredTrimIntent(plan domain.ActionPlan) (string, error) {
	value, ok := plan.Desired["trim_intent"].(string)
	if !ok || (value != "start" && value != "resume") {
		return "", fmt.Errorf("%w: trim_intent must be start or resume", ErrInvalidPlan)
	}
	return value, nil
}

func desiredBootMode(plan domain.ActionPlan) (BootMode, error) {
	value, ok := plan.Desired["boot_mode"].(string)
	if !ok {
		return "", fmt.Errorf("%w: boot_mode must be a string", ErrInvalidPlan)
	}
	mode := BootMode(value)
	if mode != BootModeSystemdBoot && mode != BootModeGRUB {
		return "", fmt.Errorf("%w: boot_mode %q is unsupported; plain GRUB is never dispatched", ErrInvalidPlan, value)
	}
	return mode, nil
}

func desiredTopology(plan domain.ActionPlan) (Topology, error) {
	value, ok := plan.Desired["topology"].(string)
	if !ok {
		return "", fmt.Errorf("%w: topology must be a string", ErrInvalidPlan)
	}
	topology := Topology(value)
	switch topology {
	case TopologySingle, TopologyMirror, TopologyRAIDZ1, TopologyRAIDZ2, TopologyRAIDZ3:
		return topology, nil
	default:
		return "", fmt.Errorf("%w: unsupported topology %q", ErrInvalidPlan, value)
	}
}

func desiredProperties(value any) (map[string]string, error) {
	result := make(map[string]string)
	switch properties := value.(type) {
	case map[string]string:
		for key, item := range properties {
			result[key] = item
		}
	case map[string]any:
		for key, item := range properties {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("property %q is not a string", key)
			}
			result[key] = text
		}
	default:
		return nil, fmt.Errorf("properties must be an object")
	}
	return result, nil
}

func appendProperties(args []string, option string, properties map[string]string) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, option, key+"="+properties[key])
	}
	return args
}

func drivePaths(refs []domain.DriveRef, minimum int) ([]string, error) {
	if len(refs) < minimum {
		return nil, fmt.Errorf("%w: at least %d target drive(s) required", ErrInvalidPlan, minimum)
	}
	if err := validateDriveRefs(refs); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		paths = append(paths, ref.ByID)
	}
	return paths, nil
}

func validateDriveRefs(refs []domain.DriveRef) error {
	seenID := make(map[string]bool)
	seenPath := make(map[string]bool)
	for _, ref := range refs {
		if ref.ID == "" || !safeIdentityText(ref.ID) || !reliableDriveRefIdentity(ref) ||
			(ref.WWN != "" && !safeIdentityText(ref.WWN)) || (ref.Serial != "" && !safeIdentityText(ref.Serial)) ||
			ref.CapacityBytes == 0 || !validSectorGeometry(ref.LogicalSectorBytes, ref.PhysicalSectorBytes) ||
			normalizeMajorMinor(ref.MajorMinor) == "" || !validByIDPath(ref.ByID) {
			return fmt.Errorf("%w: unsafe drive reference %q", ErrInvalidPlan, ref.ID)
		}
		if seenID[ref.ID] || seenPath[ref.ByID] {
			return fmt.Errorf("%w: duplicate drive reference %q", ErrInvalidPlan, ref.ID)
		}
		seenID[ref.ID] = true
		seenPath[ref.ByID] = true
	}
	return nil
}

func validateExistingTarget(plan domain.ActionPlan, needsVDev bool) error {
	if !validPoolName(plan.TargetPool.Name) || plan.TargetPool.GUID == "" {
		return fmt.Errorf("%w: existing pool target requires a valid name and GUID", ErrInvalidPlan)
	}
	if needsVDev && plan.TargetVDev.GUID == "" {
		return fmt.Errorf("%w: target vdev GUID is required", ErrInvalidPlan)
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != sha256HexLength || strings.ToLower(value) != value {
		return ErrInvalidPlan
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256ByteLength {
		return ErrInvalidPlan
	}
	return nil
}

func validatePlanID(value string) error {
	if len(value) != planIDHexLength || strings.ToLower(value) != value {
		return ErrInvalidPlan
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != planIDByteLength {
		return ErrInvalidPlan
	}
	return nil
}

const (
	sha256HexLength  = 64
	sha256ByteLength = 32
	planIDHexLength  = 32
	planIDByteLength = 16
)
