// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/bits"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func NewCreatePoolPlan(request CreatePoolRequest) (domain.ActionPlan, error) {
	return (&Builder{}).CreatePool(request)
}

func NewPrepareDiskPlan(request PrepareDiskRequest) (domain.ActionPlan, error) {
	return (&Builder{}).PrepareDisk(request)
}

func NewReplaceDiskPlan(request ReplaceDiskRequest) (domain.ActionPlan, error) {
	return (&Builder{}).ReplaceDisk(request)
}

func NewAttachMirrorPlan(request AttachMirrorRequest) (domain.ActionPlan, error) {
	return (&Builder{}).AttachMirror(request)
}

func NewExpandRAIDZPlan(request ExpandRAIDZRequest) (domain.ActionPlan, error) {
	return (&Builder{}).ExpandRAIDZ(request)
}

func NewAddVDevPlan(request AddVDevRequest) (domain.ActionPlan, error) {
	return (&Builder{}).AddVDev(request)
}

func NewAddSparePlan(request AddSpareRequest) (domain.ActionPlan, error) {
	return (&Builder{}).AddSpare(request)
}

func NewReplaceBootDiskPlan(request ReplaceBootDiskRequest) (domain.ActionPlan, error) {
	return (&Builder{}).ReplaceBootDisk(request)
}

func NewMaintenancePlan(operation Operation, request MaintenanceRequest) (domain.ActionPlan, error) {
	return (&Builder{}).Maintenance(operation, request)
}

func (builder *Builder) CreatePool(request CreatePoolRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationCreatePool)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	plan.TargetPool = domain.PoolRef{Name: request.PoolName}
	plan.Desired["topology"] = string(request.Topology)
	plan.Desired["expert"] = request.Expert
	poolProperties := cloneProperties(request.PoolProperties)
	datasetProperties := cloneProperties(request.DatasetProperties)
	applyCreateDefaults(poolProperties, datasetProperties, recommendedAshift(request.Drives))
	plan.Desired["pool_properties"] = poolProperties
	plan.Desired["dataset_properties"] = datasetProperties
	plan.ExpectedResult = fmt.Sprintf("pool %q exists with one %s data vdev", request.PoolName, request.Topology)

	if !validPoolName(request.PoolName) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool name %q is invalid", request.PoolName))
	}
	for _, pool := range request.Snapshot.Pools {
		if pool.Name == request.PoolName {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q already exists", request.PoolName))
		}
		plan.Blockers = append(plan.Blockers, poolActivityBlockers(pool)...)
	}
	validateTopology(&plan, request.Topology, len(request.Drives), request.Expert)
	refs, capacities := targetDriveRefs(&plan, request.Snapshot, request.Drives)
	plan.TargetDrives = refs
	validateMixedCapacities(&plan, capacities, request.Expert)
	validateProperties(&plan, poolProperties, datasetProperties, request.Expert)
	return finishPlan(plan)
}

func (builder *Builder) PrepareDisk(request PrepareDiskRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationPrepareDisk)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	ref, reasons := safeDriveRef(request.Snapshot, request.Drive, false)
	plan.Blockers = append(plan.Blockers, reasons...)
	if request.Drive.InUse && activeUseForPrepare(request.Drive.Use) {
		use := request.Drive.Use
		if use == "" {
			use = "unknown active use"
		}
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("drive %q is actively in use (%s)", driveLabel(request.Drive), use))
	}
	if unsafeRawOwnershipSignature(request.Drive.Use) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("drive %q contains a raw storage ownership signature that cannot be proven inactive (%s)", driveLabel(request.Drive), request.Drive.Use))
	}
	for _, pool := range request.Snapshot.Pools {
		if driveIsPoolMember(pool, request.Drive.ID) || driveUseNamesPool(request.Drive.Use, pool.Name) {
			for _, blocker := range poolActivityBlockers(pool) {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("drive %q belongs to %s", driveLabel(request.Drive), blocker))
			}
		}
	}
	plan.TargetDrives = []domain.DriveRef{ref}
	plan.ExpectedResult = "selected disk has partition-table and filesystem signatures removed; data is not securely erased"
	plan.Warnings = append(plan.Warnings, "metadata removal is destructive, is not recoverable, and is not a secure erase")
	return finishPlan(plan)
}

func (builder *Builder) ReplaceDisk(request ReplaceDiskRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationReplaceDisk)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	pool, poolOK := selectPool(&plan, request.Snapshot, request.Pool, request.AllowBoot)
	if poolOK {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	vdev, _, vdevOK := selectVDev(&plan, pool, request.OldVDev)
	if vdevOK {
		plan.TargetVDev = domain.VDevRef{Name: vdev.Name, GUID: vdev.GUID}
		if vdev.StableDriveID != "" {
			if drive, ok := findDriveByID(request.Snapshot, vdev.StableDriveID); ok {
				if ref, reasons := safeDriveRef(request.Snapshot, drive, false); len(reasons) == 0 {
					plan.SourceDrives = []domain.DriveRef{ref}
				}
			}
		}
		validateReplacementRedundancy(&plan, pool, vdev)
	}
	refs, _ := targetDriveRefs(&plan, request.Snapshot, []domain.Drive{request.NewDrive})
	plan.TargetDrives = refs
	if vdevOK && request.NewDrive.CapacityBytes < vdev.SizeBytes {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement drive %q is too small: %d bytes available, %d required", driveLabel(request.NewDrive), request.NewDrive.CapacityBytes, vdev.SizeBytes))
	}
	if vdevOK && vdev.SizeBytes == 0 {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement size authority for vdev %q is unavailable", vdev.Name))
	}
	if vdevOK && request.NewDrive.ID == vdev.StableDriveID {
		plan.Blockers = append(plan.Blockers, "replacement drive is the same stable device as the old vdev")
	}
	plan.ExpectedResult = fmt.Sprintf("vdev %q in pool %q is replaced and begins resilvering", request.OldVDev.Name, request.Pool.Name)
	return finishPlan(plan)
}

func (builder *Builder) ReplaceBootDisk(request ReplaceBootDiskRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationReplaceBootDisk)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	plan.Desired["boot_mode"] = string(request.BootMode)
	plan.ExpectedResult = fmt.Sprintf("boot disk backing vdev %q in pool %q is replaced; its ESP is registered for %s and ZFS begins resilvering", request.OldVDev.Name, request.Pool.Name, request.BootMode)
	plan.Warnings = append(plan.Warnings,
		"boot-device replacement does not repair existing boot configuration and requires reboot verification after resilvering",
		"the new disk partition table and ESP are overwritten before the final ZFS replacement command",
	)

	if !strings.HasPrefix(strings.ToLower(request.Snapshot.Host.ProxmoxVersion), "pve-manager/") || request.Snapshot.Host.ProxmoxNode == "" {
		plan.Blockers = append(plan.Blockers, "Proxmox VE host evidence is missing; pveversion and node identity are required")
	}
	if request.Snapshot.Host.ProxmoxBootMode == "" {
		plan.Blockers = append(plan.Blockers, "Proxmox boot mode is unknown or mixed; a consistent proxmox-boot-tool status is required")
	} else if request.Snapshot.Host.ProxmoxBootMode != string(request.BootMode) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("requested boot mode %q does not match freshly discovered mode %q", request.BootMode, request.Snapshot.Host.ProxmoxBootMode))
	}
	switch request.BootMode {
	case BootModeSystemdBoot, BootModeGRUB:
	case BootModePlainGRUB:
		plan.Blockers = append(plan.Blockers, "plain GRUB boot disks are unsupported; migrate to proxmox-boot-tool before replacement")
	default:
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("boot mode %q is unsupported; use systemd-boot or grub from verified proxmox-boot-tool status", request.BootMode))
	}

	pool, poolOK := selectBootPool(&plan, request.Snapshot, request.Pool)
	if poolOK {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	oldVDev, _, oldOK := selectVDev(&plan, pool, request.OldVDev)
	if oldOK {
		plan.TargetVDev = domain.VDevRef{Name: oldVDev.Name, GUID: oldVDev.GUID}
		if len(oldVDev.Children) != 0 {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("boot replacement target %q is not a leaf vdev", oldVDev.Name))
		}
	}

	sourceRef, sourceReasons := safeDriveRef(request.Snapshot, request.SourceDrive, false)
	plan.Blockers = append(plan.Blockers, sourceReasons...)
	if !wholeDiskByID(sourceRef.ByID) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("healthy source drive %q is not represented by a whole-disk /dev/disk/by-id path", driveLabel(request.SourceDrive)))
	}
	plan.SourceDrives = []domain.DriveRef{sourceRef}
	if poolOK && !driveUseNamesPool(request.SourceDrive.Use, pool.Name) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("source drive %q is not confirmed as an active member of boot pool %q", driveLabel(request.SourceDrive), pool.Name))
	}
	if !request.SourceDrive.InUse {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("source drive %q is not marked in use by discovery", driveLabel(request.SourceDrive)))
	}
	if !healthyBootSource(request.SourceDrive) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("source drive %q does not have a healthy drive-health assessment", driveLabel(request.SourceDrive)))
	}

	targetRef, targetReasons := safeDriveRef(request.Snapshot, request.NewDrive, false)
	plan.Blockers = append(plan.Blockers, targetReasons...)
	if observed, present := findDriveByID(request.Snapshot, request.NewDrive.ID); present {
		addTargetDriveHealthAssessment(&plan, request.Snapshot, observed)
	}
	if !wholeDiskByID(targetRef.ByID) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement drive %q is not represented by a whole-disk /dev/disk/by-id path", driveLabel(request.NewDrive)))
	}
	if request.NewDrive.InUse {
		use := request.NewDrive.Use
		if use == "" {
			use = "unknown active use"
		}
		if activeUseForPrepare(request.NewDrive.Use) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement drive %q is actively in use (%s)", driveLabel(request.NewDrive), use))
		} else {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement drive %q still has inactive signatures (%s); run and verify prepare-disk first", driveLabel(request.NewDrive), use))
		}
	}
	plan.TargetDrives = []domain.DriveRef{targetRef}
	if !acceptableBootTarget(request.NewDrive) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement drive %q reports failed or critical hardware health", driveLabel(request.NewDrive)))
	}

	if request.SourceDrive.ID != "" && request.SourceDrive.ID == request.NewDrive.ID {
		plan.Blockers = append(plan.Blockers, "healthy source and replacement resolve to the same stable drive")
	}
	if oldOK && oldVDev.StableDriveID != "" && oldVDev.StableDriveID == request.NewDrive.ID {
		plan.Blockers = append(plan.Blockers, "replacement drive is the same stable device as the old boot vdev")
	}
	if request.NewDrive.CapacityBytes < request.SourceDrive.CapacityBytes {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement boot drive %q is too small to clone source %q: %d bytes available, %d required", driveLabel(request.NewDrive), driveLabel(request.SourceDrive), request.NewDrive.CapacityBytes, request.SourceDrive.CapacityBytes))
	}
	// GPT partition boundaries are logical-sector offsets. Copying the table
	// across different logical sector sizes does not preserve byte offsets.
	if request.NewDrive.LogicalSectorBytes != request.SourceDrive.LogicalSectorBytes {
		plan.Blockers = append(plan.Blockers, "boot partition-table source and target logical sector sizes must match")
	}
	if oldOK && request.NewDrive.CapacityBytes < oldVDev.SizeBytes {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement boot drive %q is too small for old ZFS vdev: %d bytes available, %d required", driveLabel(request.NewDrive), request.NewDrive.CapacityBytes, oldVDev.SizeBytes))
	}
	if poolOK && oldOK {
		validateBootSibling(&plan, pool, oldVDev, request.SourceDrive)
	}
	return finishPlan(plan)
}

// Maintenance creates a yellow-risk plan for one of the narrowly allowlisted
// routine pool operations. It deliberately accepts an Operation instead of
// arbitrary argv so callers cannot smuggle a different zpool command into a
// persisted plan.
func (builder *Builder) Maintenance(operation Operation, request MaintenanceRequest) (domain.ActionPlan, error) {
	if !maintenanceOperation(operation) {
		return domain.ActionPlan{}, fmt.Errorf("%w: %q is not a routine maintenance operation", ErrInvalidPlan, operation)
	}
	plan, err := builder.base(request.Snapshot, operation)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	pool, ok := selectMaintenancePool(&plan, request.Snapshot, request.Pool)
	if ok {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	switch operation {
	case OperationScrubStart:
		plan.ExpectedResult = fmt.Sprintf("pool %q starts a scrub", request.Pool.Name)
		plan.Warnings = append(plan.Warnings, "a scrub can substantially increase pool I/O until it completes or is stopped")
	case OperationScrubStop:
		plan.ExpectedResult = fmt.Sprintf("the active scrub on pool %q is stopped", request.Pool.Name)
		plan.Warnings = append(plan.Warnings, "stopping a scrub leaves its verification incomplete; schedule another full scrub")
	case OperationTrimStart:
		if request.Pool.Trim.State == "suspended" {
			plan.Desired["trim_intent"] = "resume"
			plan.ExpectedResult = fmt.Sprintf("pool %q resumes its suspended manual TRIM pass", request.Pool.Name)
			plan.Warnings = append(plan.Warnings, "running zpool trim without flags resumes a suspended TRIM rather than starting a new pass")
		} else {
			plan.Desired["trim_intent"] = "start"
			plan.ExpectedResult = fmt.Sprintf("pool %q starts a manual TRIM pass", request.Pool.Name)
		}
		plan.Warnings = append(plan.Warnings, "manual TRIM can increase latency and applies only to devices that support discard")
	case OperationTrimStop:
		plan.ExpectedResult = fmt.Sprintf("the active manual TRIM on pool %q is suspended", request.Pool.Name)
		plan.Warnings = append(plan.Warnings, "zpool trim -s suspends rather than cancels the current pass; running trim again resumes it")
	case OperationClearErrors:
		plan.ExpectedResult = fmt.Sprintf("recorded device errors for pool %q are cleared", request.Pool.Name)
		plan.Warnings = append(plan.Warnings, "clearing error counters does not repair hardware, restore data, or resolve the cause of an error")
	}
	if ok {
		plan.Blockers = append(plan.Blockers, maintenancePlanBlockers(plan, pool)...)
	}
	return finishPlan(plan)
}

func (builder *Builder) AttachMirror(request AttachMirrorRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationAttachMirror)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	pool, poolOK := selectPool(&plan, request.Snapshot, request.Pool, request.AllowBoot)
	if poolOK {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	vdev, parentType, vdevOK := selectVDev(&plan, pool, request.ExistingVDev)
	if vdevOK {
		plan.TargetVDev = domain.VDevRef{Name: vdev.Name, GUID: vdev.GUID}
		if strings.HasPrefix(parentType, "raidz") || strings.HasPrefix(parentType, "draid") {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("vdev %q is already a member of %s and cannot be converted to a mirror", vdev.Name, parentType))
		}
		if len(vdev.Children) > 0 {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("vdev %q is not a leaf device", vdev.Name))
		}
		if !online(vdev.State) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("vdev %q must be ONLINE before attaching a mirror", vdev.Name))
		}
		if !online(pool.State) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q must be ONLINE before attaching a mirror", pool.Name))
		}
	}
	refs, _ := targetDriveRefs(&plan, request.Snapshot, []domain.Drive{request.NewDrive})
	plan.TargetDrives = refs
	if vdevOK && request.NewDrive.CapacityBytes < vdev.SizeBytes {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("mirror drive %q is too small: %d bytes available, %d required", driveLabel(request.NewDrive), request.NewDrive.CapacityBytes, vdev.SizeBytes))
	}
	if vdevOK && vdev.SizeBytes == 0 {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("attach size authority for vdev %q is unavailable", vdev.Name))
	}
	if vdevOK && request.NewDrive.ID == vdev.StableDriveID {
		plan.Blockers = append(plan.Blockers, "new mirror drive is the same stable device as the existing vdev")
	}
	plan.ExpectedResult = fmt.Sprintf("vdev %q in pool %q becomes a mirror and begins resilvering", request.ExistingVDev.Name, request.Pool.Name)
	return finishPlan(plan)
}

func (builder *Builder) ExpandRAIDZ(request ExpandRAIDZRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationExpandRAIDZ)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	if !openZFSAtLeast23(request.Snapshot.Host.OpenZFSVersion) {
		version := request.Snapshot.Host.OpenZFSVersion
		if version == "" {
			version = "unknown"
		}
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("RAIDZ expansion requires OpenZFS 2.3 or newer; discovered version is %q", version))
	}
	pool, poolOK := selectPool(&plan, request.Snapshot, request.Pool, request.AllowBoot)
	if poolOK {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	vdev, _, vdevOK := selectVDev(&plan, pool, request.RAIDZVDev)
	if vdevOK {
		plan.TargetVDev = domain.VDevRef{Name: vdev.Name, GUID: vdev.GUID}
		kind := vdevTopology(vdev)
		if !strings.HasPrefix(string(kind), "raidz") {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("vdev %q is %s, not RAIDZ", vdev.Name, kind))
		}
		if !isTopLevelVDev(pool, vdev.GUID) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("RAIDZ vdev %q is not a top-level data vdev", vdev.Name))
		}
		minimum := minimumChildSize(vdev)
		if minimum == 0 {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("RAIDZ vdev %q has incomplete child-size authority", vdev.Name))
		} else if request.NewDrive.CapacityBytes < minimum {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("expansion drive %q is too small: %d bytes available, %d required", driveLabel(request.NewDrive), request.NewDrive.CapacityBytes, minimum))
		}
		if !online(pool.State) || !allVDevsOnline(vdev) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q and every child of RAIDZ vdev %q must be ONLINE before expansion", pool.Name, vdev.Name))
		}
	}
	refs, _ := targetDriveRefs(&plan, request.Snapshot, []domain.Drive{request.NewDrive})
	plan.TargetDrives = refs
	if vdevOK && vdevContainsDriveID(vdev, request.NewDrive.ID) {
		plan.Blockers = append(plan.Blockers, "expansion drive is already a member of the selected RAIDZ vdev")
	}
	plan.Warnings = append(plan.Warnings, "RAIDZ expansion is a long-running topology change and does not immediately rewrite existing blocks to the new stripe width")
	plan.ExpectedResult = fmt.Sprintf("RAIDZ vdev %q in pool %q has one additional device and begins expansion", request.RAIDZVDev.Name, request.Pool.Name)
	return finishPlan(plan)
}

func openZFSAtLeast23(version string) bool {
	version = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(version, "zfs-"), "v"))
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minorText := parts[1]
	for index, character := range minorText {
		if character < '0' || character > '9' {
			minorText = minorText[:index]
			break
		}
	}
	minor, minorErr := strconv.Atoi(minorText)
	return majorErr == nil && minorErr == nil && (major > 2 || major == 2 && minor >= 3)
}

func (builder *Builder) AddVDev(request AddVDevRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationAddVDev)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	pool, poolOK := selectPool(&plan, request.Snapshot, request.Pool, request.AllowBoot)
	if poolOK {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	plan.Desired["topology"] = string(request.Topology)
	validateTopology(&plan, request.Topology, len(request.Drives), false)
	refs, capacities := targetDriveRefs(&plan, request.Snapshot, request.Drives)
	plan.TargetDrives = refs
	validateMixedCapacities(&plan, capacities, false)
	if poolOK {
		validateMatchingVDev(&plan, pool, request.Topology, len(request.Drives))
		if !online(pool.State) || !allDataVDevsOnline(pool) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q and all data vdevs must be ONLINE before adding a top-level vdev", pool.Name))
		}
	}
	plan.Warnings = append(plan.Warnings, "adding a top-level vdev changes pool geometry and generally cannot be undone")
	plan.ExpectedResult = fmt.Sprintf("pool %q contains one additional matching %s data vdev", request.Pool.Name, request.Topology)
	return finishPlan(plan)
}

func (builder *Builder) AddSpare(request AddSpareRequest) (domain.ActionPlan, error) {
	plan, err := builder.base(request.Snapshot, OperationAddSpare)
	if err != nil {
		return domain.ActionPlan{}, err
	}
	pool, poolOK := selectPool(&plan, request.Snapshot, request.Pool, request.AllowBoot)
	if poolOK {
		plan.TargetPool = domain.PoolRef{Name: pool.Name, GUID: pool.GUID}
	}
	if len(request.Drives) == 0 {
		plan.Blockers = append(plan.Blockers, "at least one spare drive is required")
	}
	refs, _ := targetDriveRefs(&plan, request.Snapshot, request.Drives)
	plan.TargetDrives = refs
	if poolOK {
		required, complete := maximumDataLeafSize(pool)
		if !complete {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q has incomplete data-leaf size authority; spare suitability cannot be proven", pool.Name))
		} else {
			for _, drive := range request.Drives {
				if drive.CapacityBytes < required {
					plan.Blockers = append(plan.Blockers, fmt.Sprintf("spare drive %q is too small for the pool's largest data leaf: %d bytes available, %d required", driveLabel(drive), drive.CapacityBytes, required))
				}
			}
		}
	}
	plan.ExpectedResult = fmt.Sprintf("pool %q has %d additional hot spare(s)", request.Pool.Name, len(request.Drives))
	return finishPlan(plan)
}

func (builder *Builder) base(snapshot domain.Snapshot, operation Operation) (domain.ActionPlan, error) {
	now := time.Now().UTC()
	if builder != nil && builder.Now != nil {
		now = builder.Now().UTC()
	}
	ttl := DefaultPlanTTL
	if builder != nil && builder.TTL > 0 {
		ttl = builder.TTL
	}
	if ttl > MaxPlanTTL {
		ttl = MaxPlanTTL
	}
	newID := randomID
	if builder != nil && builder.NewID != nil {
		newID = builder.NewID
	}
	id, err := newID()
	if err != nil {
		return domain.ActionPlan{}, fmt.Errorf("create plan id: %w", err)
	}
	return domain.ActionPlan{
		APIVersion:          domain.PlanAPIVersion,
		Kind:                PlanKind,
		ID:                  id,
		CreatedAt:           now,
		ExpiresAt:           now.Add(ttl),
		HostMachineID:       snapshot.Host.MachineID,
		Operation:           string(operation),
		Risk:                operationRisk(operation),
		Desired:             make(map[string]any),
		TopologyFingerprint: TopologyFingerprint(snapshot),
		SnapshotFingerprint: SnapshotFingerprint(snapshot),
		Blockers:            []string{}, Warnings: []string{}, Commands: []domain.PlannedCommand{},
	}, nil
}

func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func finishPlan(plan domain.ActionPlan) (domain.ActionPlan, error) {
	plan.Blockers = sortedUnique(plan.Blockers)
	plan.Warnings = sortedUnique(plan.Warnings)
	commands, err := RebuildCommands(plan)
	if err != nil {
		plan.Blockers = sortedUnique(append(plan.Blockers, "safe command preview unavailable: "+err.Error()))
		plan.Commands = []domain.PlannedCommand{}
	} else {
		plan.Commands = commands
	}
	if plan.Risk == domain.RiskDestructive {
		plan.Confirmation = expectedRedConfirmation(plan)
	} else {
		plan.Confirmation = ""
	}
	return plan, nil
}

func expectedRedConfirmation(plan domain.ActionPlan) string {
	challenge := ""
	if len(plan.TargetDrives) == 1 && safeChallengeToken(plan.TargetDrives[0].Serial) {
		challenge = plan.TargetDrives[0].Serial
	} else {
		challenge = Fingerprint(plan)[:12]
	}
	return "yes " + challenge
}

func safeChallengeToken(value string) bool {
	if len(value) < 4 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._-", character)) {
			return false
		}
	}
	return true
}

func targetDriveRefs(plan *domain.ActionPlan, snapshot domain.Snapshot, drives []domain.Drive) ([]domain.DriveRef, []uint64) {
	refs := make([]domain.DriveRef, 0, len(drives))
	capacities := make([]uint64, 0, len(drives))
	seen := make(map[string]struct{})
	for _, drive := range drives {
		ref, reasons := safeDriveRef(snapshot, drive, true)
		plan.Blockers = append(plan.Blockers, reasons...)
		if observed, present := findDriveByID(snapshot, drive.ID); present {
			addTargetDriveHealthAssessment(plan, snapshot, observed)
		}
		if _, duplicate := seen[drive.ID]; drive.ID != "" && duplicate {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("drive %q is selected more than once", driveLabel(drive)))
		}
		seen[drive.ID] = struct{}{}
		refs = append(refs, ref)
		capacities = append(capacities, drive.CapacityBytes)
	}
	return refs, capacities
}

// addTargetDriveHealthAssessment uses health only as an additional safety
// signal. A positively failed assessment is a hard blocker. Missing,
// unsupported, sleeping, or merely warning telemetry remains prominent but
// does not strand an otherwise valid no-wake maintenance workflow.
func addTargetDriveHealthAssessment(plan *domain.ActionPlan, snapshot domain.Snapshot, drive domain.Drive) {
	plan.Blockers = append(plan.Blockers, targetDriveHealthBlockers(snapshot, drive)...)
	scope := "drive/" + drive.ID
	for _, finding := range snapshot.Findings {
		if finding.Scope != scope || finding.Severity == domain.SeverityOK || finding.Severity == domain.SeverityInfo || finding.Severity == domain.SeverityCritical {
			continue
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("target drive %q health is %s: %s", driveLabel(drive), finding.Severity, finding.Title))
	}
	if drive.Health.FreshAt.IsZero() {
		reason := "has no current recognized SMART health sample"
		if drive.Health.Sleeping {
			reason = "is sleeping; SMART health was not refreshed"
		} else if drive.Health.Unsupported {
			reason = "does not expose supported SMART health data"
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("target drive %q %s; verify the hardware before apply", driveLabel(drive), reason))
	} else if drive.Health.Severity == domain.SeverityWarning || drive.Health.Severity == domain.SeverityUnknown {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("target drive %q SMART severity is %s; review its health details before apply", driveLabel(drive), drive.Health.Severity))
	}
}

func targetDriveHealthBlockers(snapshot domain.Snapshot, drive domain.Drive) []string {
	var blockers []string
	if drive.Health.SMARTPassed != nil && !*drive.Health.SMARTPassed {
		blockers = append(blockers, fmt.Sprintf("target drive %q reports a failed SMART overall-health assessment", driveLabel(drive)))
	}
	if drive.Health.Severity == domain.SeverityCritical {
		blockers = append(blockers, fmt.Sprintf("target drive %q reports critical hardware health", driveLabel(drive)))
	}
	scope := "drive/" + drive.ID
	for _, finding := range snapshot.Findings {
		if finding.Scope == scope && finding.Severity == domain.SeverityCritical {
			blockers = append(blockers, fmt.Sprintf("target drive %q has a critical health finding: %s", driveLabel(drive), finding.Title))
		}
	}
	return sortedUnique(blockers)
}

func operationRequiresHealthyTarget(operation Operation) bool {
	switch operation {
	case OperationCreatePool, OperationReplaceDisk, OperationAttachMirror,
		OperationExpandRAIDZ, OperationAddVDev, OperationAddSpare,
		OperationReplaceBootDisk:
		return true
	default:
		return false
	}
}

func safeDriveRef(snapshot domain.Snapshot, drive domain.Drive, requireUnused bool) (domain.DriveRef, []string) {
	label := driveLabel(drive)
	ref := domain.DriveRef{
		ID: drive.ID, WWN: drive.WWN, Serial: drive.Serial, ByID: drive.ByID,
		MajorMinor: driveMajorMinor(drive), CapacityBytes: drive.CapacityBytes,
		LogicalSectorBytes: drive.LogicalSectorBytes, PhysicalSectorBytes: drive.PhysicalSectorBytes,
		Bay: drive.Bay.Label,
	}
	var blockers []string
	if drive.ID == "" || !reliableDriveIdentity(drive) {
		blockers = append(blockers, fmt.Sprintf("drive %q has no stable hardware identity", label))
	}
	if drive.Bay.Confidence == "conflict" {
		blockers = append(blockers, fmt.Sprintf("drive %q has a conflicting physical bay mapping; remap and verify the bay before planning", label))
	}
	if !safeIdentityText(drive.ID) || (drive.WWN != "" && !safeIdentityText(drive.WWN)) || (drive.Serial != "" && !safeIdentityText(drive.Serial)) {
		blockers = append(blockers, fmt.Sprintf("drive %q identity contains unsafe control characters", label))
	}
	if !validByIDPath(drive.ByID) {
		blockers = append(blockers, fmt.Sprintf("drive %q does not have a safe /dev/disk/by-id path", label))
	}
	if drive.CapacityBytes == 0 {
		blockers = append(blockers, fmt.Sprintf("drive %q has unknown capacity", label))
	}
	if !validSectorGeometry(drive.LogicalSectorBytes, drive.PhysicalSectorBytes) {
		blockers = append(blockers, fmt.Sprintf("drive %q has unknown or invalid logical/physical sector geometry (%d/%d)", label, drive.LogicalSectorBytes, drive.PhysicalSectorBytes))
	}
	if ref.MajorMinor == "" {
		blockers = append(blockers, fmt.Sprintf("drive %q has no verified major:minor device number", label))
	}
	observed, present := findDriveByID(snapshot, drive.ID)
	identityCount := driveIDCount(snapshot, drive.ID)
	if !present || drive.ID == "" {
		blockers = append(blockers, fmt.Sprintf("drive %q is not present in the discovery snapshot", label))
	} else if identityCount != 1 {
		blockers = append(blockers, fmt.Sprintf("drive %q stable identity resolves to %d devices", label, identityCount))
	} else if DriveIdentityFingerprint(observed) != DriveIdentityFingerprint(drive) {
		blockers = append(blockers, fmt.Sprintf("drive %q does not match its discovery-snapshot identity", label))
	}
	if drive.WWN == "" && drive.Serial != "" && serialCount(snapshot, drive.Serial) > 1 {
		blockers = append(blockers, fmt.Sprintf("drive %q has a duplicate serial and no WWN", label))
	}
	if requireUnused && (drive.InUse || protectedUse(drive.Use)) {
		use := drive.Use
		if use == "" {
			use = "unknown active use"
		}
		blockers = append(blockers, fmt.Sprintf("drive %q is already in use (%s)", label, use))
	}
	return ref, blockers
}

func driveIDCount(snapshot domain.Snapshot, id string) int {
	count := 0
	for _, drive := range snapshot.Drives {
		if drive.ID == id {
			count++
		}
	}
	return count
}

func validSectorGeometry(logical, physical uint64) bool {
	return logical >= 512 && logical <= 64*1024 && logical&(logical-1) == 0 &&
		physical >= logical && physical <= 64*1024 && physical&(physical-1) == 0 && physical%logical == 0
}

func safeIdentityText(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// driveMajorMinor accepts a future domain.Drive.MajorMinor field without
// coupling this package to a domain migration, then falls back to a canonical
// /sys/dev/block component or stat(2) of the discovered block devnode.
func driveMajorMinor(drive domain.Drive) string {
	if result := normalizeMajorMinor(drive.MajorMinor); result != "" {
		return result
	}
	for _, component := range strings.Split(filepath.Clean(drive.SysfsPath), string(filepath.Separator)) {
		if result := normalizeMajorMinor(component); result != "" {
			return result
		}
	}
	for _, path := range []string{drive.DevNode, drive.ByID} {
		if path == "" {
			continue
		}
		var stat syscall.Stat_t
		if err := syscall.Stat(path, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFBLK {
			continue
		}
		device := uint64(stat.Rdev)
		major := ((device >> 8) & 0xfff) | ((device >> 32) & ^uint64(0xfff))
		minor := (device & 0xff) | ((device >> 12) & ^uint64(0xff))
		return fmt.Sprintf("%d:%d", major, minor)
	}
	return ""
}

func normalizeMajorMinor(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return ""
	}
	major, majorErr := strconv.ParseUint(parts[0], 10, 32)
	minor, minorErr := strconv.ParseUint(parts[1], 10, 32)
	if majorErr != nil || minorErr != nil {
		return ""
	}
	if major == 0 && minor == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", major, minor)
}

func validByIDPath(path string) bool {
	return strings.HasPrefix(path, "/dev/disk/by-id/") && path != "/dev/disk/by-id/" &&
		filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n\t")
}

func protectedUse(use string) bool {
	use = strings.ToLower(use)
	for _, word := range []string{"root", "boot", "mounted", "mountpoint", "swap", "lvm", "mdraid", "md array", "zfs", "pool member"} {
		if strings.Contains(use, word) {
			return true
		}
	}
	return false
}

func activeUseForPrepare(use string) bool {
	use = strings.ToLower(strings.TrimSpace(use))
	if use == "" {
		return true
	}
	for _, marker := range []string{" mounted at ", " is held by ", "zfs pool ", "active swap", "[swap]", "root", "boot"} {
		if strings.Contains(use, marker) {
			return true
		}
	}
	// Discovery describes an inactive on-disk signature as "contains TYPE".
	// Every other non-empty use description is conservatively active.
	for _, item := range strings.Split(use, ", ") {
		if !strings.Contains(item, " contains ") {
			return true
		}
	}
	return false
}

// Some raw formats can back an imported pool, cluster OSD, encrypted mapping,
// software RAID, or volume manager without being mounted. A generic wipe tool
// cannot prove these signatures stale, so prepare-disk fails closed. Ordinary
// unmounted filesystem signatures remain eligible for the explicit red wipe.
func unsafeRawOwnershipSignature(use string) bool {
	use = strings.ToLower(use)
	for _, marker := range []string{"zfs_member", "ceph_bluestore", "lvm2_member", "linux_raid_member", "crypto_luks", "bitlocker", "swap"} {
		if strings.Contains(use, marker) {
			return true
		}
	}
	return false
}

func driveLabel(drive domain.Drive) string {
	for _, value := range []string{drive.ID, drive.WWN, drive.Serial, drive.ByID} {
		if value != "" {
			return value
		}
	}
	return "unknown"
}

func findDriveByID(snapshot domain.Snapshot, id string) (domain.Drive, bool) {
	for _, drive := range snapshot.Drives {
		if drive.ID == id {
			return drive, true
		}
	}
	return domain.Drive{}, false
}

func serialCount(snapshot domain.Snapshot, serial string) int {
	count := 0
	for _, drive := range snapshot.Drives {
		if drive.Serial == serial {
			count++
		}
	}
	return count
}

func selectPool(plan *domain.ActionPlan, snapshot domain.Snapshot, requested domain.Pool, allowBoot bool) (domain.Pool, bool) {
	if requested.Name == "" || requested.GUID == "" {
		plan.Blockers = append(plan.Blockers, "target pool requires both name and GUID")
		return domain.Pool{}, false
	}
	for _, pool := range snapshot.Pools {
		if pool.GUID == requested.GUID {
			if pool.Name != requested.Name {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool GUID %q is now named %q, not %q", requested.GUID, pool.Name, requested.Name))
				return domain.Pool{}, false
			}
			if pool.BootCritical {
				if allowBoot {
					plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q is boot-critical; this generic planner cannot validate ESP or Proxmox boot state, so a dedicated Proxmox boot-device plan is required", pool.Name))
				} else {
					plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q is boot-critical and requires the dedicated Proxmox boot-device workflow", pool.Name))
				}
			}
			plan.Blockers = append(plan.Blockers, poolActivityBlockers(pool)...)
			return pool, true
		}
	}
	plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q with GUID %q is not present", requested.Name, requested.GUID))
	return domain.Pool{}, false
}

func selectMaintenancePool(plan *domain.ActionPlan, snapshot domain.Snapshot, requested domain.Pool) (domain.Pool, bool) {
	if requested.Name == "" || requested.GUID == "" {
		plan.Blockers = append(plan.Blockers, "maintenance target requires both pool name and GUID")
		return domain.Pool{}, false
	}
	for _, pool := range snapshot.Pools {
		if pool.GUID != requested.GUID {
			continue
		}
		if pool.Name != requested.Name {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool GUID %q is now named %q, not %q", requested.GUID, pool.Name, requested.Name))
			return domain.Pool{}, false
		}
		if pool.BootCritical {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("pool %q is boot-critical; routine maintenance may affect host latency or availability", pool.Name))
		}
		return pool, true
	}
	plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q with GUID %q is not present", requested.Name, requested.GUID))
	return domain.Pool{}, false
}

func maintenancePoolBlockers(operation Operation, pool domain.Pool) []string {
	var blockers []string
	// These operations require an imported, administrable pool. DEGRADED is
	// accepted so an operator can verify or clear a degraded pool; unavailable
	// and suspended pools require diagnosis outside this routine workflow.
	if !online(pool.State) && !strings.EqualFold(pool.State, "DEGRADED") {
		blockers = append(blockers, fmt.Sprintf("pool %q state %q is not safe for routine maintenance", pool.Name, pool.State))
	}
	activity := classifyScanActivity(pool.Scan)
	active := activity == scanActivityActive
	scanName := strings.ToLower(strings.TrimSpace(pool.Scan.Function))
	switch operation {
	case OperationScrubStart:
		if activity == scanActivityUnknown {
			blockers = append(blockers, fmt.Sprintf("pool %q scan activity is unknown; collect a complete zpool status before starting a scrub", pool.Name))
		} else if active {
			if scanName == "" {
				scanName = "unknown scan"
			}
			blockers = append(blockers, fmt.Sprintf("pool %q already has an active %s; wait for it to finish or stop it explicitly", pool.Name, scanName))
		}
		blockers = append(blockers, scrubTrimConflictBlockers(pool)...)
	case OperationScrubStop:
		if !active || scanName != "scrub" {
			blockers = append(blockers, fmt.Sprintf("pool %q does not have a confirmed active scrub to stop", pool.Name))
		}
	case OperationTrimStop:
		if pool.Trim.Supported == nil || !*pool.Trim.Supported || pool.Trim.State != "active" {
			blockers = append(blockers, fmt.Sprintf("pool %q does not have a confirmed active manual TRIM to stop", pool.Name))
		}
	case OperationTrimStart:
		if activity == scanActivityUnknown {
			blockers = append(blockers, fmt.Sprintf("pool %q scan activity is unknown; collect a complete zpool status before starting or resuming TRIM", pool.Name))
		} else if active {
			if scanName == "" {
				scanName = "unknown scan"
			}
			blockers = append(blockers, fmt.Sprintf("pool %q already has an active %s; wait for it to finish or stop it explicitly", pool.Name, scanName))
		}
		switch pool.Trim.State {
		case "untrimmed", "complete", "canceled", "suspended":
			if pool.Trim.Supported == nil || !*pool.Trim.Supported {
				blockers = append(blockers, fmt.Sprintf("pool %q does not have consistently supported TRIM leaves", pool.Name))
			}
		case "active":
			blockers = append(blockers, fmt.Sprintf("pool %q already has an active manual TRIM", pool.Name))
		case "unsupported":
			blockers = append(blockers, fmt.Sprintf("pool %q reports TRIM unsupported", pool.Name))
		case "mixed", "unknown", "":
			blockers = append(blockers, fmt.Sprintf("pool %q TRIM state is %s; all leaf states must be consistently known before starting or resuming TRIM", pool.Name, valueOrUnknownTrimState(pool.Trim.State)))
		default:
			blockers = append(blockers, fmt.Sprintf("pool %q reports unrecognized TRIM state %q", pool.Name, pool.Trim.State))
		}
	}
	return blockers
}

func scrubTrimConflictBlockers(pool domain.Pool) []string {
	var blockers []string
	switch pool.Trim.State {
	case "active":
		blockers = append(blockers, fmt.Sprintf("pool %q already has an active manual TRIM; wait for it to finish or suspend it before starting a scrub", pool.Name))
	case "suspended":
		blockers = append(blockers, fmt.Sprintf("pool %q has a suspended manual TRIM pass; resume and finish it before starting a scrub", pool.Name))
	case "mixed":
		active, uncertain := trimLeafActivity(pool.VDevs)
		if active {
			blockers = append(blockers, fmt.Sprintf("pool %q has active or suspended TRIM on one or more leaf vdevs", pool.Name))
		} else if uncertain {
			blockers = append(blockers, fmt.Sprintf("pool %q has mixed TRIM state with unknown leaf activity; a scrub cannot be coordinated safely", pool.Name))
		}
	case "unknown", "":
		blockers = append(blockers, fmt.Sprintf("pool %q TRIM activity is unknown; collect successful zpool status -t data before starting a scrub", pool.Name))
	case "untrimmed", "complete", "canceled", "unsupported":
		// Confirmed not active.
	default:
		blockers = append(blockers, fmt.Sprintf("pool %q has unrecognized TRIM state %q", pool.Name, pool.Trim.State))
	}
	return blockers
}

func maintenancePlanBlockers(plan domain.ActionPlan, pool domain.Pool) []string {
	blockers := maintenancePoolBlockers(Operation(plan.Operation), pool)
	if Operation(plan.Operation) != OperationTrimStart {
		return blockers
	}
	intent, err := desiredTrimIntent(plan)
	if err != nil {
		return append(blockers, err.Error())
	}
	switch intent {
	case "resume":
		if pool.Trim.State != "suspended" {
			blockers = append(blockers, fmt.Sprintf("pool %q TRIM state changed from suspended; this plan is authorized only to resume that pass", pool.Name))
		}
	case "start":
		if pool.Trim.State == "suspended" {
			blockers = append(blockers, fmt.Sprintf("pool %q now has a suspended TRIM; this plan is authorized to start a new pass, not resume one", pool.Name))
		}
	}
	return blockers
}

func valueOrUnknownTrimState(state string) string {
	if strings.TrimSpace(state) == "" {
		return "unknown"
	}
	return state
}

type scanActivity uint8

const (
	scanActivityUnknown scanActivity = iota
	scanActivityInactive
	scanActivityActive
)

// classifyScanActivity is deliberately fail-closed. Empty and unfamiliar
// fields do not prove that a scrub or resilver is idle.
func classifyScanActivity(scan domain.Scan) scanActivity {
	function := strings.ToLower(strings.TrimSpace(scan.Function))
	state := strings.ToLower(strings.TrimSpace(scan.State))
	state = strings.NewReplacer("_", " ", "-", " ").Replace(state)
	state = strings.Join(strings.Fields(state), " ")
	switch function {
	case "":
		if state == "none" || state == "not requested" {
			return scanActivityInactive
		}
		return scanActivityUnknown
	case "none":
		if state == "none" || state == "not requested" {
			return scanActivityInactive
		}
		return scanActivityUnknown
	case "scrub", "resilver":
		// Known scan functions are classified by an explicit state below.
	default:
		return scanActivityUnknown
	}
	switch state {
	case "scanning", "running", "active", "in progress", "paused":
		return scanActivityActive
	case "finished", "complete", "completed", "canceled", "cancelled":
		return scanActivityInactive
	default:
		return scanActivityUnknown
	}
}

func scanActive(scan domain.Scan) bool {
	return classifyScanActivity(scan) == scanActivityActive
}

func selectBootPool(plan *domain.ActionPlan, snapshot domain.Snapshot, requested domain.Pool) (domain.Pool, bool) {
	if requested.Name == "" || requested.GUID == "" {
		plan.Blockers = append(plan.Blockers, "boot pool requires both name and GUID")
		return domain.Pool{}, false
	}
	for _, pool := range snapshot.Pools {
		if pool.GUID != requested.GUID {
			continue
		}
		if pool.Name != requested.Name {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("boot pool GUID %q is now named %q, not %q", requested.GUID, pool.Name, requested.Name))
			return domain.Pool{}, false
		}
		if !pool.BootCritical {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q is not marked boot-critical; boot-device workflow is unavailable", pool.Name))
		}
		if !online(pool.State) && !strings.EqualFold(pool.State, "DEGRADED") {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("boot pool %q state %q is not safe for guided replacement", pool.Name, pool.State))
		}
		plan.Blockers = append(plan.Blockers, poolActivityBlockers(pool)...)
		return pool, true
	}
	plan.Blockers = append(plan.Blockers, fmt.Sprintf("boot pool %q with GUID %q is not present", requested.Name, requested.GUID))
	return domain.Pool{}, false
}

func poolActivityBlockers(pool domain.Pool) []string {
	var blockers []string
	switch classifyScanActivity(pool.Scan) {
	case scanActivityActive:
		kind := strings.ToLower(strings.TrimSpace(pool.Scan.Function))
		if kind == "" {
			kind = "unknown scan"
		}
		blockers = append(blockers, fmt.Sprintf("pool %q has an active %s", pool.Name, kind))
	case scanActivityUnknown:
		blockers = append(blockers, fmt.Sprintf("pool %q scan activity is unknown; collect a complete zpool status before a destructive lifecycle operation", pool.Name))
	}
	switch pool.Trim.State {
	case "active":
		blockers = append(blockers, fmt.Sprintf("pool %q has an active manual TRIM", pool.Name))
	case "suspended":
		blockers = append(blockers, fmt.Sprintf("pool %q has a suspended manual TRIM pass; resume or cancel it before a destructive lifecycle operation", pool.Name))
	case "mixed":
		active, uncertain := trimLeafActivity(pool.VDevs)
		if active {
			blockers = append(blockers, fmt.Sprintf("pool %q has active or suspended TRIM on one or more leaf vdevs", pool.Name))
		} else if uncertain {
			blockers = append(blockers, fmt.Sprintf("pool %q has mixed TRIM state with unknown leaf activity", pool.Name))
		}
	case "unknown", "":
		blockers = append(blockers, fmt.Sprintf("pool %q TRIM activity is unknown; collect successful zpool status -t data first", pool.Name))
	case "untrimmed", "complete", "canceled", "unsupported":
		// Confirmed not active.
	default:
		blockers = append(blockers, fmt.Sprintf("pool %q has unrecognized TRIM state %q", pool.Name, pool.Trim.State))
	}
	return blockers
}

func trimLeafActivity(vdevs []domain.VDev) (active, uncertain bool) {
	for _, vdev := range vdevs {
		if len(vdev.Children) > 0 {
			childActive, childUncertain := trimLeafActivity(vdev.Children)
			active = active || childActive
			uncertain = uncertain || childUncertain
			continue
		}
		if vdev.Trim.Supported == nil || vdev.Trim.State == "" || vdev.Trim.State == "unknown" {
			uncertain = true
			continue
		}
		if !*vdev.Trim.Supported {
			continue
		}
		if vdev.Trim.State == "active" || vdev.Trim.State == "suspended" {
			active = true
		}
	}
	return active, uncertain
}

func driveIsPoolMember(pool domain.Pool, driveID string) bool {
	if driveID == "" {
		return false
	}
	for _, vdev := range pool.VDevs {
		if vdevContainsDriveID(vdev, driveID) {
			return true
		}
	}
	return false
}

func vdevContainsDriveID(vdev domain.VDev, driveID string) bool {
	if driveID == "" {
		return false
	}
	if vdev.StableDriveID == driveID {
		return true
	}
	for _, child := range vdev.Children {
		if vdevContainsDriveID(child, driveID) {
			return true
		}
	}
	return false
}

func validateBootSibling(plan *domain.ActionPlan, pool domain.Pool, oldVDev domain.VDev, source domain.Drive) {
	parent, found := parentVDev(pool, oldVDev.GUID)
	if !found || vdevTopology(parent) != TopologyMirror {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("old boot vdev %q is not a member of a discovered mirror", oldVDev.Name))
		return
	}
	if !fixedByIDPartition(oldVDev.Path, ProxmoxZFSPartition) {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("old boot vdev %q is not pinned to persistent Proxmox ZFS partition 3", oldVDev.Name))
	}
	matching := 0
	for _, sibling := range parent.Children {
		if sibling.GUID == oldVDev.GUID || sibling.StableDriveID != source.ID {
			continue
		}
		matching++
		if !online(sibling.State) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("source sibling vdev %q is %s, not ONLINE", sibling.Name, sibling.State))
		}
		if len(sibling.Children) != 0 {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("source sibling vdev %q is not a leaf", sibling.Name))
		}
		if !fixedByIDPartition(sibling.Path, ProxmoxZFSPartition) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("source sibling vdev %q is not pinned to persistent Proxmox ZFS partition 3", sibling.Name))
		}
	}
	if matching != 1 {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("source drive %q resolves to %d ONLINE-sibling candidates beside old vdev %q", driveLabel(source), matching, oldVDev.Name))
	}
}

func driveUseNamesPool(use, pool string) bool {
	return strings.Contains(strings.ToLower(use), "zfs pool "+strings.ToLower(pool))
}

func healthyBootSource(drive domain.Drive) bool {
	if drive.Health.SMARTPassed == nil || !*drive.Health.SMARTPassed || drive.Health.FreshAt.IsZero() {
		return false
	}
	return drive.Health.Severity == domain.SeverityOK || drive.Health.Severity == domain.SeverityInfo
}

func acceptableBootTarget(drive domain.Drive) bool {
	return (drive.Health.SMARTPassed == nil || *drive.Health.SMARTPassed) && drive.Health.Severity != domain.SeverityCritical
}

func wholeDiskByID(path string) bool {
	if !validByIDPath(path) {
		return false
	}
	base := filepath.Base(path)
	marker := strings.LastIndex(base, "-part")
	if marker < 0 {
		return true
	}
	suffix := base[marker+len("-part"):]
	return suffix == "" || strings.Trim(suffix, "0123456789") != ""
}

func partitionByID(path string, number int) (string, error) {
	if !wholeDiskByID(path) || (number != ProxmoxESPPartition && number != ProxmoxZFSPartition) {
		return "", fmt.Errorf("%w: unsafe Proxmox partition reference", ErrInvalidPlan)
	}
	return path + "-part" + strconv.Itoa(number), nil
}

func fixedByIDPartition(path string, number int) bool {
	suffix := "-part" + strconv.Itoa(number)
	return strings.HasSuffix(path, suffix) && wholeDiskByID(strings.TrimSuffix(path, suffix))
}

func selectVDev(plan *domain.ActionPlan, pool domain.Pool, requested domain.VDev) (domain.VDev, string, bool) {
	if pool.GUID == "" {
		return domain.VDev{}, "", false
	}
	if requested.GUID == "" {
		plan.Blockers = append(plan.Blockers, "target vdev requires a GUID")
		return domain.VDev{}, "", false
	}
	for _, candidate := range pool.VDevs {
		if found, parent, ok := findVDev(candidate, requested.GUID, ""); ok {
			if requested.Name != "" && found.Name != requested.Name {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("vdev GUID %q is now named %q, not %q", requested.GUID, found.Name, requested.Name))
				return domain.VDev{}, "", false
			}
			return found, parent, true
		}
	}
	plan.Blockers = append(plan.Blockers, fmt.Sprintf("vdev %q with GUID %q is not present in pool %q", requested.Name, requested.GUID, pool.Name))
	return domain.VDev{}, "", false
}

func findVDev(root domain.VDev, guid, parentType string) (domain.VDev, string, bool) {
	if root.GUID == guid {
		return root, parentType, true
	}
	for _, child := range root.Children {
		if found, parent, ok := findVDev(child, guid, string(vdevTopology(root))); ok {
			return found, parent, true
		}
	}
	return domain.VDev{}, "", false
}

func validateTopology(plan *domain.ActionPlan, topology Topology, count int, expert bool) {
	minimum := map[Topology]int{TopologySingle: 1, TopologyMirror: 2, TopologyRAIDZ1: 3, TopologyRAIDZ2: 4, TopologyRAIDZ3: 5}
	needed, supported := minimum[topology]
	if !supported {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("topology %q is unsupported", topology))
		return
	}
	if count < needed {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("topology %s requires at least %d drives; %d selected", topology, needed, count))
	}
	if topology == TopologySingle && count > 1 {
		plan.Blockers = append(plan.Blockers, "single topology requires exactly one drive; multiple bare drives would create separate striped top-level vdevs")
	}
	if topology == TopologySingle || topology == TopologyRAIDZ1 {
		if !expert {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("topology %s requires expert mode because it has limited redundancy", topology))
		} else {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("expert override accepts limited-redundancy topology %s", topology))
		}
	}
}

func validateMixedCapacities(plan *domain.ActionPlan, capacities []uint64, expert bool) {
	if len(capacities) < 2 {
		return
	}
	minimum, maximum := capacities[0], capacities[0]
	for _, capacity := range capacities[1:] {
		minimum = min(minimum, capacity)
		maximum = max(maximum, capacity)
	}
	if minimum == 0 || float64(maximum-minimum)/float64(maximum) <= .01 {
		return
	}
	message := fmt.Sprintf("selected drive capacities differ by more than 1%% (%d to %d bytes)", minimum, maximum)
	if expert {
		plan.Warnings = append(plan.Warnings, "expert override: "+message)
	} else {
		plan.Blockers = append(plan.Blockers, message+"; expert mode is required")
	}
}

func validateMatchingVDev(plan *domain.ActionPlan, pool domain.Pool, requested Topology, width int) {
	foundData := false
	for _, vdev := range pool.VDevs {
		if vdev.Class != "" && vdev.Class != "data" {
			continue
		}
		foundData = true
		kind := vdevTopology(vdev)
		if kind != requested {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("new top-level vdev %s does not match existing %s vdev %q", requested, kind, vdev.Name))
		}
		existingWidth := len(vdev.Children)
		if kind == TopologySingle && existingWidth == 0 {
			existingWidth = 1
		}
		if existingWidth != width {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("new top-level vdev width %d does not match existing width %d for vdev %q", width, existingWidth, vdev.Name))
		}
	}
	if !foundData {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool %q has no discoverable top-level data vdev to match", pool.Name))
	}
}

func vdevTopology(vdev domain.VDev) Topology {
	value := strings.ToLower(vdev.Type)
	if value == "" {
		value = strings.ToLower(vdev.Name)
	}
	for _, topology := range []Topology{TopologyRAIDZ3, TopologyRAIDZ2, TopologyRAIDZ1, TopologyMirror} {
		if strings.HasPrefix(value, string(topology)) {
			return topology
		}
	}
	if len(vdev.Children) == 0 {
		return TopologySingle
	}
	return Topology(value)
}

func isTopLevelVDev(pool domain.Pool, guid string) bool {
	for _, vdev := range pool.VDevs {
		if vdev.GUID == guid && (vdev.Class == "" || vdev.Class == "data") {
			return true
		}
	}
	return false
}

func minimumChildSize(vdev domain.VDev) uint64 {
	if len(vdev.Children) == 0 {
		return 0
	}
	minimum := uint64(0)
	for _, child := range vdev.Children {
		if child.SizeBytes == 0 {
			return 0
		}
		if minimum == 0 || child.SizeBytes < minimum {
			minimum = child.SizeBytes
		}
	}
	return minimum
}

func maximumDataLeafSize(pool domain.Pool) (uint64, bool) {
	maximum := uint64(0)
	found := false
	var walk func(domain.VDev) bool
	walk = func(vdev domain.VDev) bool {
		if len(vdev.Children) == 0 {
			found = true
			if vdev.SizeBytes == 0 {
				return false
			}
			if vdev.SizeBytes > maximum {
				maximum = vdev.SizeBytes
			}
			return true
		}
		for _, child := range vdev.Children {
			if !walk(child) {
				return false
			}
		}
		return true
	}
	for _, vdev := range pool.VDevs {
		if vdev.Class != "" && vdev.Class != "data" {
			continue
		}
		if !walk(vdev) {
			return 0, false
		}
	}
	return maximum, found && maximum > 0
}

func online(state string) bool { return strings.EqualFold(state, "ONLINE") }

func allVDevsOnline(vdev domain.VDev) bool {
	if !online(vdev.State) {
		return false
	}
	for _, child := range vdev.Children {
		if !allVDevsOnline(child) {
			return false
		}
	}
	return true
}

func allDataVDevsOnline(pool domain.Pool) bool {
	for _, vdev := range pool.VDevs {
		if (vdev.Class == "" || vdev.Class == "data") && !allVDevsOnline(vdev) {
			return false
		}
	}
	return true
}

func validateReplacementRedundancy(plan *domain.ActionPlan, pool domain.Pool, target domain.VDev) {
	if len(target.Children) != 0 {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("replacement target %q is not a leaf vdev", target.Name))
		return
	}
	parent, found := parentVDev(pool, target.GUID)
	if !found {
		if !online(target.State) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("single-disk vdev %q is not ONLINE; no surviving redundancy is available for replacement", target.Name))
		}
		return
	}
	kind := vdevTopology(parent)
	otherUnavailable := 0
	otherOnline := 0
	for _, child := range parent.Children {
		if child.GUID == target.GUID {
			continue
		}
		if online(child.State) {
			otherOnline++
		} else {
			otherUnavailable++
		}
	}
	switch kind {
	case TopologyMirror:
		if otherOnline == 0 {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("mirror containing %q has no other ONLINE leaf to resilver from", target.Name))
		}
	case TopologyRAIDZ1, TopologyRAIDZ2, TopologyRAIDZ3:
		parity := map[Topology]int{TopologyRAIDZ1: 1, TopologyRAIDZ2: 2, TopologyRAIDZ3: 3}[kind]
		if otherUnavailable >= parity {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("%s containing %q has %d other unavailable leaf(s), leaving inadequate redundancy for replacement", kind, target.Name, otherUnavailable))
		}
	default:
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("parent topology %q for vdev %q is not supported for guided replacement", kind, target.Name))
	}
}

func parentVDev(pool domain.Pool, guid string) (domain.VDev, bool) {
	for _, root := range pool.VDevs {
		if parent, found := parentWithin(root, guid); found {
			return parent, true
		}
	}
	return domain.VDev{}, false
}

func parentWithin(root domain.VDev, guid string) (domain.VDev, bool) {
	for _, child := range root.Children {
		if child.GUID == guid {
			return root, true
		}
		if parent, found := parentWithin(child, guid); found {
			return parent, true
		}
	}
	return domain.VDev{}, false
}

func cloneProperties(properties map[string]string) map[string]string {
	result := make(map[string]string, len(properties)+3)
	for key, value := range properties {
		result[key] = value
	}
	return result
}

func applyCreateDefaults(pool, dataset map[string]string, ashift int) {
	if _, ok := pool["ashift"]; !ok {
		pool["ashift"] = strconv.Itoa(ashift)
	}
	if _, ok := dataset["compression"]; !ok {
		dataset["compression"] = "lz4"
	}
	if _, ok := dataset["dedup"]; !ok {
		dataset["dedup"] = "off"
	}
}

func recommendedAshift(drives []domain.Drive) int {
	ashift := 12
	for _, drive := range drives {
		sector := drive.PhysicalSectorBytes
		if sector > 0 && sector&(sector-1) == 0 {
			ashift = max(ashift, bits.TrailingZeros64(sector))
		}
	}
	return min(ashift, 16)
}

func validateProperties(plan *domain.ActionPlan, pool, dataset map[string]string, expert bool) {
	for key, value := range pool {
		if !validPoolProperty(key, value) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("pool property %s=%q is not in the safe create allowlist", key, value))
		}
	}
	for key, value := range dataset {
		if !validDatasetProperty(key, value) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("dataset property %s=%q is not in the safe create allowlist", key, value))
		}
		if key == "dedup" && value != "off" {
			plan.Warnings = append(plan.Warnings, "dedup is enabled and may require substantial memory")
			if !expert {
				plan.Blockers = append(plan.Blockers, "dedup requires expert mode")
			}
		}
	}
	if err := validateEncryptionProperties(dataset); err != nil {
		plan.Blockers = append(plan.Blockers, err.Error())
	}
}

func validateEncryptionProperties(properties map[string]string) error {
	encryption := properties["encryption"]
	keyFormat, hasFormat := properties["keyformat"]
	keyLocation, hasLocation := properties["keylocation"]
	if encryption == "" || encryption == "off" {
		if hasFormat || hasLocation {
			return fmt.Errorf("keyformat/keylocation require encryption to be enabled")
		}
		return nil
	}
	if !hasFormat || !hasLocation {
		return fmt.Errorf("encrypted pool creation requires both keyformat and keylocation")
	}
	if keyLocation == "prompt" && keyFormat != "passphrase" {
		return fmt.Errorf("interactive keylocation=prompt requires keyformat=passphrase")
	}
	return nil
}

func validPoolProperty(key, value string) bool {
	switch key {
	case "ashift":
		number, err := strconv.Atoi(value)
		return err == nil && number >= 9 && number <= 16
	case "autotrim", "autoexpand", "autoreplace":
		return value == "on" || value == "off"
	default:
		return false
	}
}

func validDatasetProperty(key, value string) bool {
	switch key {
	case "compression":
		return validCompression(value)
	case "dedup":
		return value == "off" || value == "on" || value == "verify"
	case "atime", "relatime":
		return value == "on" || value == "off"
	case "xattr":
		return value == "on" || value == "off" || value == "sa"
	case "acltype":
		return value == "off" || value == "posix" || value == "nfsv4"
	case "sync":
		return value == "standard" || value == "always"
	case "recordsize":
		return validRecordSize(value)
	case "encryption":
		return slices.Contains([]string{"off", "on", "aes-128-ccm", "aes-192-ccm", "aes-256-ccm", "aes-128-gcm", "aes-192-gcm", "aes-256-gcm"}, value)
	case "keyformat":
		return value == "passphrase" || value == "hex" || value == "raw"
	case "keylocation":
		if value == "prompt" {
			return true
		}
		if !strings.HasPrefix(value, "file:///") || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
		path := strings.TrimPrefix(value, "file://")
		return filepath.IsAbs(path) && filepath.Clean(path) == path
	default:
		return false
	}
}

func validCompression(value string) bool {
	if slices.Contains([]string{"off", "on", "lz4", "zle", "gzip", "zstd"}, value) {
		return true
	}
	if strings.HasPrefix(value, "gzip-") {
		number, err := strconv.Atoi(strings.TrimPrefix(value, "gzip-"))
		return err == nil && number >= 1 && number <= 9
	}
	if strings.HasPrefix(value, "zstd-") {
		number, err := strconv.Atoi(strings.TrimPrefix(value, "zstd-"))
		return err == nil && number >= 1 && number <= 19
	}
	return false
}

func validRecordSize(value string) bool {
	units := uint64(1)
	numberPart := value
	if strings.HasSuffix(value, "K") {
		units, numberPart = 1024, strings.TrimSuffix(value, "K")
	} else if strings.HasSuffix(value, "M") {
		units, numberPart = 1024*1024, strings.TrimSuffix(value, "M")
	}
	number, err := strconv.ParseUint(numberPart, 10, 64)
	if err != nil || number > ^uint64(0)/units {
		return false
	}
	bytes := number * units
	return bytes >= 512 && bytes <= 16*1024*1024 && bytes&(bytes-1) == 0
}

func validPoolName(name string) bool {
	if len(name) == 0 || len(name) > 255 || !((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) {
		return false
	}
	for _, character := range name {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune("_-:.", character) {
			return false
		}
	}
	lower := strings.ToLower(name)
	if slices.Contains([]string{"mirror", "raidz", "draid", "spare", "log"}, lower) {
		return false
	}
	return true
}

func sortedUnique(values []string) []string {
	result := slices.Clone(values)
	sort.Strings(result)
	result = slices.Compact(result)
	if result == nil {
		return []string{}
	}
	return result
}
