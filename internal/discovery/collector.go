// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// Collect builds a best-effort point-in-time snapshot.  Environmental and
// command failures are represented in Snapshot.Errors; the returned Go error
// is reserved for invalid setup and caller cancellation.
func (c *Collector) Collect(ctx context.Context, options Options) (domain.Snapshot, error) {
	options = options.withDefaults()
	snapshot := domain.Snapshot{
		APIVersion: domain.SnapshotAPIVersion,
		Generated:  c.now(),
		Pools:      []domain.Pool{},
		Datasets:   []domain.Dataset{},
		Drives:     []domain.Drive{},
		Findings:   []domain.Finding{},
		Errors:     []domain.SourceError{},
	}
	if c.runner == nil {
		return snapshot, errors.New("discovery: nil Runner")
	}
	state := collectionState{collector: c, options: options, snapshot: &snapshot}

	state.collectHost(ctx)
	state.collectZFS(ctx)
	state.collectRootMount()
	if err := ctx.Err(); err != nil {
		state.finish()
		return snapshot, err
	}
	state.collectDrives(ctx)
	state.collectARC()
	if err := ctx.Err(); err != nil {
		state.finish()
		return snapshot, err
	}

	markBootPoolsFromDatasets(snapshot.Pools, snapshot.Datasets)
	correlateVDevs(c.fs, snapshot.Pools, snapshot.Drives)
	state.finish()
	return snapshot, nil
}

type collectionState struct {
	collector *Collector
	options   Options
	snapshot  *domain.Snapshot
}

func (state *collectionState) finish() {
	sort.Slice(state.snapshot.Errors, func(i, j int) bool {
		if state.snapshot.Errors[i].Source != state.snapshot.Errors[j].Source {
			return state.snapshot.Errors[i].Source < state.snapshot.Errors[j].Source
		}
		if state.snapshot.Errors[i].Kind != state.snapshot.Errors[j].Kind {
			return state.snapshot.Errors[i].Kind < state.snapshot.Errors[j].Kind
		}
		return state.snapshot.Errors[i].Message < state.snapshot.Errors[j].Message
	})
	state.snapshot.Partial = false
	for _, sourceError := range state.snapshot.Errors {
		if sourceError.Partial {
			state.snapshot.Partial = true
			break
		}
	}
	state.snapshot.Findings = EvaluateFindings(*state.snapshot, state.options.Thresholds)
}

func (state *collectionState) collectHost(ctx context.Context) {
	hostname, err := state.collector.hostname()
	if err != nil {
		state.addError("host.hostname", classifyFilesystemError(err), err.Error(), true)
	} else {
		state.snapshot.Host.Hostname = hostname
	}
	if value, err := state.collector.fs.ReadFile(state.options.MachineIDPath); err != nil {
		state.addError("host.machine-id", classifyFilesystemError(err), err.Error(), true)
	} else {
		state.snapshot.Host.MachineID = strings.TrimSpace(string(value))
	}
	if value, err := state.collector.fs.ReadFile(state.options.OSReleasePath); err != nil {
		state.addError("host.os-release", classifyFilesystemError(err), err.Error(), true)
	} else {
		state.snapshot.Host.OS = parseOSRelease(value)
	}
	if value, err := state.collector.fs.ReadFile(state.options.KernelReleasePath); err != nil {
		state.addError("host.kernel", classifyFilesystemError(err), err.Error(), true)
	} else {
		state.snapshot.Host.Kernel = strings.TrimSpace(string(value))
	}

	primary := state.run(ctx, "zpool", "version", "-j")
	versionFound := false
	if successful(primary) {
		if version, parseErr := parseZpoolVersion(primary.Stdout); parseErr == nil {
			state.snapshot.Host.OpenZFSVersion = version
			versionFound = true
		}
	}
	if !versionFound && classifyError(primary) == "missing-tool" {
		state.addResultError("zpool.version", primary, true)
	} else if !versionFound {
		fallback := state.run(ctx, "zpool", "--version")
		if successful(fallback) {
			if version, parseErr := parseZpoolVersion(fallback.Stdout); parseErr == nil {
				state.snapshot.Host.OpenZFSVersion = version
				versionFound = true
				state.addError("zpool.version.json", "unsupported", "JSON version output unavailable; used text compatibility output", true)
			}
		}
		if !versionFound {
			state.addResultError("zpool.version", bestFailure(primary, fallback), true)
		}
	}

	// Proxmox is an optional host integration. Its absence is normal on generic
	// Linux and therefore never makes an otherwise complete ZFS snapshot
	// partial. Presence is retained so boot-specific planning can fail closed.
	pve := state.run(ctx, "pveversion")
	if successful(pve) {
		state.snapshot.Host.ProxmoxVersion = parsePVEVersion(pve.Stdout)
		state.snapshot.Host.ProxmoxNode = state.snapshot.Host.Hostname
		boot := state.run(ctx, "proxmox-boot-tool", "status")
		if successful(boot) {
			combined := strings.TrimSpace(string(boot.Stdout)) + "\n" + strings.TrimSpace(string(boot.Stderr))
			if mode, parseErr := ParseProxmoxBootMode(combined); parseErr == nil {
				state.snapshot.Host.ProxmoxBootMode = mode
			} else {
				state.addError("proxmox.boot-status", "parse", parseErr.Error(), false)
			}
		} else {
			state.addResultError("proxmox.boot-status", boot, false)
		}
	} else if classifyError(pve) != "missing-tool" {
		state.addResultError("proxmox.version", pve, false)
	}
}

func parsePVEVersion(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if value := strings.TrimSpace(line); value != "" {
			return value
		}
	}
	return "unknown"
}

func (state *collectionState) collectZFS(ctx context.Context) {
	pools := make(map[string]*domain.Pool)

	statusPrimary := state.run(ctx, "zpool", "status", "-j", "--json-int", "-P", "-t")
	statusRecords, statusParseErr := parseSuccessful(statusPrimary, parseZpoolStatusJSON)
	statusAvailable := statusParseErr == nil
	statusIdentityAuthoritative := successful(statusPrimary) && statusParseErr == nil
	emptyStatusConfirmed := false
	if statusParseErr != nil && classifyError(statusPrimary) == "missing-tool" {
		state.addResultError("zpool.status", statusPrimary, true)
	} else if statusParseErr != nil {
		fallback := state.run(ctx, "zpool", "status", "-P", "-p", "-t")
		fallbackStatus, fallbackErr := parseSuccessful(fallback, parseZpoolStatusText)
		if fallbackErr == nil {
			statusRecords = fallbackStatus
			statusAvailable = true
			emptyStatusConfirmed = errors.Is(statusParseErr, errEmptyPoolsJSON) && len(fallbackStatus) == 0 && isNoObjects(fallback)
			if !confirmedEmptyPoolJSON(statusParseErr, fallback, len(fallbackStatus), 0) {
				state.addFallbackError("zpool.status", statusPrimary, statusParseErr)
			}
		} else {
			state.addCollectionFailure("zpool.status", statusPrimary, statusParseErr, fallback, fallbackErr)
		}
	}
	if statusAvailable {
		mergePools(pools, statusRecords)
	}

	listPrimary := state.run(ctx, "zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health")
	listRecords, listParseErr := parseSuccessful(listPrimary, parseZpoolListJSON)
	// OpenZFS 2.4 emits no JSON at all for an empty zpool list. A blank
	// response alone is insufficient: independently confirmed empty status
	// must precede it, and all later sources still have to agree.
	if emptyStatusConfirmed && emptySuccessfulResult(listPrimary) {
		listRecords, listParseErr = []domain.Pool{}, nil
	}
	if listParseErr == nil && statusIdentityAuthoritative {
		listParseErr = poolSliceCoverageError(pools, listRecords, "zpool list JSON")
	}
	listAvailable := listParseErr == nil
	if listParseErr != nil && classifyError(listPrimary) == "missing-tool" {
		state.addResultError("zpool.list", listPrimary, true)
	} else if listParseErr != nil {
		fallback := state.run(ctx, "zpool", "list", "-H", "-p", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health")
		fallbackList, fallbackErr := parseSuccessful(fallback, parseZpoolListText)
		if emptyStatusConfirmed && errors.Is(listParseErr, errEmptyPoolsJSON) && emptySuccessfulResult(fallback) {
			fallbackList, fallbackErr = []domain.Pool{}, nil
		}
		if fallbackErr == nil && statusIdentityAuthoritative {
			fallbackErr = poolSliceCoverageError(pools, fallbackList, "zpool list text fallback")
		}
		if fallbackErr == nil {
			listRecords = fallbackList
			listAvailable = true
			if !confirmedEmptyPoolJSON(listParseErr, fallback, len(fallbackList), len(pools)) {
				state.addFallbackError("zpool.list", listPrimary, listParseErr)
			}
		} else {
			state.addCollectionFailure("zpool.list", listPrimary, listParseErr, fallback, fallbackErr)
		}
	}
	if listAvailable {
		mergePools(pools, listRecords)
	}

	getPrimary := state.run(ctx, "zpool", "get", "-j", "--json-int", "all")
	poolProperties, getParseErr := parseSuccessfulMap(getPrimary, parseZpoolGetJSON)
	primaryPoolProperties := poolProperties
	if getParseErr == nil && (statusIdentityAuthoritative || listAvailable) {
		getParseErr = poolMapCoverageError(pools, poolProperties, "zpool get JSON")
	}
	getAvailable := getParseErr == nil
	if getParseErr != nil && classifyError(getPrimary) == "missing-tool" {
		state.addResultError("zpool.get", getPrimary, true)
	} else if getParseErr != nil {
		fallback := state.run(ctx, "zpool", "get", "-H", "-p", "-o", "name,property,value,source", "all")
		fallbackProperties, fallbackErr := parseSuccessfulMap(fallback, parseZpoolGetText)
		if emptyStatusConfirmed && listAvailable && len(pools) == 0 && errors.Is(getParseErr, errEmptyPoolsJSON) && emptySuccessfulResult(fallback) {
			fallbackProperties, fallbackErr = map[string]map[string]string{}, nil
		}
		if fallbackErr == nil && (statusIdentityAuthoritative || listAvailable) {
			fallbackErr = poolMapCoverageError(pools, fallbackProperties, "zpool get text fallback")
		}
		if fallbackErr == nil {
			poolProperties = fallbackProperties
			getAvailable = true
			if !confirmedEmptyPoolJSON(getParseErr, fallback, len(fallbackProperties), len(pools)) {
				state.addFallbackError("zpool.get", getPrimary, getParseErr)
			}
		} else {
			state.addCollectionFailure("zpool.get", getPrimary, getParseErr, fallback, fallbackErr)
		}
	}
	if getAvailable {
		applyPoolProperties(pools, poolProperties)
	}
	// Boot evidence is conservative: even an otherwise identity-inconsistent
	// source may prove that a same-named discovered pool participates in boot.
	// It may never clear an existing boot-critical classification.
	markBootPoolsFromPropertyEvidence(pools, primaryPoolProperties)

	if statusIdentityAuthoritative {
		state.addPoolSliceCoverageError("zpool.status", pools, statusRecords)
	} else if statusAvailable {
		state.addPoolSliceNameCoverageError("zpool.status", pools, statusRecords)
	}
	if listAvailable {
		state.addPoolSliceCoverageError("zpool.list", pools, listRecords)
	}
	if getAvailable {
		state.addPoolMapCoverageError("zpool.get", pools, poolProperties)
	}

	iostat := state.run(ctx, "zpool", "iostat", "-H", "-p")
	if successful(iostat) || isNoObjects(iostat) {
		if err := applyIOStatText(pools, iostat.Stdout); err != nil {
			state.addError("zpool.iostat", "parse", err.Error(), true)
		}
	} else {
		state.addResultError("zpool.iostat", iostat, true)
	}

	state.snapshot.Pools = finalizePools(pools)

	zfsPrimary := state.run(ctx, "zfs", "get", "-j", "--json-int", "all")
	datasets, zfsParseErr := parseSuccessful(zfsPrimary, parseZFSGetJSON)
	primaryDatasets := datasets
	if zfsParseErr == nil {
		zfsParseErr = datasetCoverageError(pools, poolProperties, datasets, "zfs get JSON")
	}
	if zfsParseErr == nil {
		state.snapshot.Datasets = datasets
	} else if classifyError(zfsPrimary) == "missing-tool" {
		state.addResultError("zfs.get", zfsPrimary, true)
	} else {
		fallback := state.run(ctx, "zfs", "get", "-H", "-p", "-o", "name,property,value,source", "all")
		fallbackDatasets, fallbackErr := parseSuccessful(fallback, parseZFSGetText)
		if emptyStatusConfirmed && listAvailable && getAvailable && len(pools) == 0 && errors.Is(zfsParseErr, errEmptyDatasetsJSON) && emptySuccessfulResult(fallback) {
			fallbackDatasets, fallbackErr = []domain.Dataset{}, nil
		}
		if fallbackErr == nil {
			fallbackErr = datasetCoverageError(pools, poolProperties, fallbackDatasets, "zfs get text fallback")
		}
		if fallbackErr == nil {
			state.snapshot.Datasets = fallbackDatasets
			if !confirmedEmptyDatasetJSON(zfsParseErr, fallback, len(fallbackDatasets), len(pools)) {
				state.addFallbackError("zfs.get", zfsPrimary, zfsParseErr)
			}
		} else {
			state.addCollectionFailure("zfs.get", zfsPrimary, zfsParseErr, fallback, fallbackErr)
		}
	}
	markBootPoolsFromDatasets(state.snapshot.Pools, primaryDatasets)
}

func confirmedEmptyPoolJSON(parseErr error, fallback Result, observed, expected int) bool {
	return errors.Is(parseErr, errEmptyPoolsJSON) && observed == 0 && expected == 0 && (isNoObjects(fallback) || emptySuccessfulResult(fallback))
}

func confirmedEmptyDatasetJSON(parseErr error, fallback Result, observed, poolCount int) bool {
	return errors.Is(parseErr, errEmptyDatasetsJSON) && observed == 0 && poolCount == 0 && (isNoObjects(fallback) || emptySuccessfulResult(fallback))
}

func emptySuccessfulResult(result Result) bool {
	return successful(result) && len(bytes.TrimSpace(result.Stdout)) == 0 && len(bytes.TrimSpace(result.Stderr)) == 0
}

func poolSliceCoverageError(expected map[string]*domain.Pool, observed []domain.Pool, source string) error {
	identities := make(map[string]string, len(observed))
	for _, pool := range observed {
		if pool.Name != "" {
			identities[pool.Name] = pool.GUID
		}
	}
	return poolIdentityCoverageError(expected, identities, source)
}

func poolMapCoverageError(expected map[string]*domain.Pool, observed map[string]map[string]string, source string) error {
	identities := make(map[string]string, len(observed))
	for name, properties := range observed {
		if name != "" {
			identities[name] = strings.TrimSpace(properties["guid"])
		}
	}
	return poolIdentityCoverageError(expected, identities, source)
}

func poolIdentityCoverageError(expected map[string]*domain.Pool, observed map[string]string, source string) error {
	problems := make([]string, 0)
	for name, pool := range expected {
		guid, present := observed[name]
		switch {
		case !present:
			problems = append(problems, fmt.Sprintf("missing %q", name))
		case pool.GUID == "" || guid == "":
			problems = append(problems, fmt.Sprintf("unverifiable GUID for %q", name))
		case pool.GUID != guid:
			problems = append(problems, fmt.Sprintf("GUID mismatch for %q (%s != %s)", name, pool.GUID, guid))
		}
	}
	for name := range observed {
		if _, present := expected[name]; !present {
			problems = append(problems, fmt.Sprintf("unexpected %q", name))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%s pool identity coverage differs: %s", source, strings.Join(problems, "; "))
}

func (state *collectionState) addPoolSliceCoverageError(source string, expected map[string]*domain.Pool, observed []domain.Pool) {
	if err := poolSliceCoverageError(expected, observed, source); err != nil {
		state.addError(source, "inconsistent", err.Error(), true)
	}
}

func (state *collectionState) addPoolSliceNameCoverageError(source string, expected map[string]*domain.Pool, observed []domain.Pool) {
	expectedNames := make(map[string]bool, len(expected))
	for name := range expected {
		expectedNames[name] = true
	}
	observedNames := make(map[string]bool, len(observed))
	for _, pool := range observed {
		observedNames[pool.Name] = true
	}
	var differences []string
	for name := range expectedNames {
		if !observedNames[name] {
			differences = append(differences, fmt.Sprintf("missing %q", name))
		}
	}
	for name := range observedNames {
		if !expectedNames[name] {
			differences = append(differences, fmt.Sprintf("unexpected %q", name))
		}
	}
	if len(differences) > 0 {
		sort.Strings(differences)
		state.addError(source, "inconsistent", source+" pool name coverage differs: "+strings.Join(differences, "; "), true)
	}
}

func (state *collectionState) addPoolMapCoverageError(source string, expected map[string]*domain.Pool, observed map[string]map[string]string) {
	if err := poolMapCoverageError(expected, observed, source); err != nil {
		state.addError(source, "inconsistent", err.Error(), true)
	}
}

func datasetCoverageError(pools map[string]*domain.Pool, poolProperties map[string]map[string]string, datasets []domain.Dataset, source string) error {
	datasetNames := make(map[string]bool, len(datasets))
	datasetPools := make(map[string]bool, len(datasets))
	for _, dataset := range datasets {
		datasetNames[dataset.Name] = true
		poolName := datasetPoolName(dataset.Name)
		if poolName == "" {
			return fmt.Errorf("%s returned dataset with no pool identity: %q", source, dataset.Name)
		}
		if _, present := pools[poolName]; !present {
			return fmt.Errorf("%s returned dataset %q for undiscovered pool %q", source, dataset.Name, poolName)
		}
		datasetPools[poolName] = true
	}

	poolNames := make([]string, 0, len(pools))
	for name := range pools {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)
	for _, poolName := range poolNames {
		if !datasetPools[poolName] {
			return fmt.Errorf("%s omitted all datasets for imported pool %q", source, poolName)
		}
		if !datasetNames[poolName] {
			return fmt.Errorf("%s omitted root dataset %q", source, poolName)
		}
		bootfs := strings.TrimSpace(poolProperties[poolName]["bootfs"])
		if bootfs != "" && bootfs != "-" && !strings.EqualFold(bootfs, "none") {
			if datasetPoolName(bootfs) != poolName {
				return fmt.Errorf("%s reported cross-pool bootfs dataset %q for pool %q", source, bootfs, poolName)
			}
			if !datasetNames[bootfs] {
				return fmt.Errorf("%s omitted bootfs dataset %q for pool %q", source, bootfs, poolName)
			}
		}
	}
	return nil
}

func datasetPoolName(name string) string {
	name = strings.TrimSpace(name)
	if separator := strings.IndexAny(name, "/@"); separator >= 0 {
		name = name[:separator]
	}
	return name
}

func markBootPoolsFromPropertyEvidence(pools map[string]*domain.Pool, properties map[string]map[string]string) {
	for name, values := range properties {
		bootfs := strings.TrimSpace(values["bootfs"])
		if bootfs == "" || bootfs == "-" || strings.EqualFold(bootfs, "none") {
			continue
		}
		if pool := pools[name]; pool != nil {
			pool.BootCritical = true
		}
	}
}

func (state *collectionState) collectDrives(ctx context.Context) {
	lsblk := state.run(ctx, "lsblk", "--json", "--bytes", "--paths", "--output", "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,VENDOR,REV,ROTA,TRAN,LOG-SEC,PHY-SEC,MOUNTPOINTS,FSTYPE,PKNAME")
	if !successful(lsblk) {
		state.addResultError("lsblk", lsblk, true)
		return
	}
	drives, err := parseLSBLK(lsblk.Stdout)
	if err != nil {
		state.addError("lsblk", "parse", err.Error(), true)
		return
	}

	if err := applyByID(state.collector.fs, state.options.ByIDDir, drives); err != nil {
		state.addError("drive.by-id", classifyFilesystemError(err), err.Error(), true)
	}
	// by-path represents a physical controller/port route and is therefore a
	// useful inferred bay label. It is optional and never overrides a verified
	// mapping (verified mappings are merged by the inventory layer later).
	_ = applyByPath(state.collector.fs, state.options.ByPathDir, drives)
	state.enrichSysfs(drives)
	state.collectSysfsHolders(drives)
	state.collectUdev(ctx, drives)
	for index := range drives {
		finalizeDriveIdentity(&drives[index])
	}
	state.markDuplicateIdentities(drives)
	if !state.options.SkipSMART {
		state.collectSMART(ctx, drives)
	}
	sort.Slice(drives, func(i, j int) bool {
		if drives[i].ID != drives[j].ID {
			return drives[i].ID < drives[j].ID
		}
		return drives[i].DevNode < drives[j].DevNode
	})
	state.snapshot.Drives = drives
}

func (state *collectionState) collectSysfsHolders(drives []domain.Drive) {
	const classBlock = "/sys/class/block"
	if _, err := state.collector.fs.ReadDir(classBlock); err != nil {
		// The program supports Linux only. Missing sysfs authority must never be
		// interpreted as proof that a destructive target has no live holders.
		state.addError("drive.holders", classifyFilesystemError(err), err.Error(), true)
		return
	}

	for index := range drives {
		nodes := drives[index].BlockNodes
		if len(nodes) == 0 && drives[index].DevNode != "" {
			nodes = []string{filepath.Base(drives[index].DevNode)}
		}
		for _, node := range nodes {
			holdersPath := filepath.Join(classBlock, filepath.Base(node), "holders")
			entries, err := state.collector.fs.ReadDir(holdersPath)
			if err != nil {
				state.addError("drive.holders", classifyFilesystemError(err), fmt.Sprintf("read %s: %v", holdersPath, err), true)
				continue
			}
			holders := make([]string, 0, len(entries))
			for _, entry := range entries {
				name := filepath.Base(strings.TrimSpace(entry.Name()))
				if name == "" || name == "." || name == string(filepath.Separator) {
					continue
				}
				holders = append(holders, name)
			}
			sort.Strings(holders)
			for _, holder := range holders {
				usage := fmt.Sprintf("/dev/%s is held by %s (sysfs)", filepath.Base(node), holder)
				appendDriveUse(&drives[index], usage)
			}
		}
	}
}

func appendDriveUse(drive *domain.Drive, usage string) {
	usage = strings.TrimSpace(usage)
	if usage == "" {
		return
	}
	drive.InUse = true
	for _, existing := range strings.Split(drive.Use, ", ") {
		if existing == usage {
			return
		}
	}
	if drive.Use == "" {
		drive.Use = usage
		return
	}
	drive.Use += ", " + usage
}

func (state *collectionState) enrichSysfs(drives []domain.Drive) {
	for index := range drives {
		name := filepath.Base(drives[index].DevNode)
		classPath := filepath.Join("/sys/class/block", name)
		if resolved, err := state.collector.fs.EvalSymlinks(classPath); err == nil {
			drives[index].SysfsPath = resolved
		}
		if drives[index].WWN != "" {
			continue
		}
		for _, attribute := range []string{"wwid", "device/nguid", "device/eui"} {
			if value, err := state.collector.fs.ReadFile(filepath.Join(classPath, attribute)); err == nil {
				if identifier := normalizeIdentifier(string(value)); identifier != "" && identifier != "0" {
					drives[index].WWN = identifier
					break
				}
			}
		}
	}
}

func (state *collectionState) collectUdev(ctx context.Context, drives []domain.Drive) {
	for index := range drives {
		if err := ctx.Err(); err != nil {
			return
		}
		result := state.run(ctx, "udevadm", "info", "--query=property", "--name", drives[index].DevNode)
		if !successful(result) {
			state.addResultError("udev:"+drives[index].DevNode, result, true)
			if classifyError(result) == "missing-tool" {
				return
			}
			continue
		}
		enrichDriveFromUdev(&drives[index], parseUdevProperties(result.Stdout))
	}
}

func (state *collectionState) collectSMART(ctx context.Context, drives []domain.Drive) {
	for index := range drives {
		if err := ctx.Err(); err != nil {
			return
		}
		args, supported := smartReadArgs(drives[index], state.options.WakeSleepingDisks)
		if !supported {
			message := "SMART skipped: standby-safe access is not established for this transport; use an explicit --wake refresh"
			drives[index].Health = domain.DriveHealth{Severity: domain.SeverityUnknown, Unsupported: true, Message: message}
			state.addError("smart:"+drives[index].ID, "unsupported", message, true)
			continue
		}
		result := state.run(ctx, "smartctl", args...)
		if result.TimedOut || result.ExitCode < 0 || result.ExitCode > 255 ||
			errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded) ||
			(result.Err != nil && (len(result.Stdout) == 0 || result.ExitCode == 0)) {
			state.addResultError("smart:"+drives[index].ID, result, true)
			drives[index].Health = domain.DriveHealth{Severity: domain.SeverityUnknown, Unsupported: classifyError(result) == "missing-tool", Message: result.ErrString()}
			if classifyError(result) == "missing-tool" {
				for remaining := index + 1; remaining < len(drives); remaining++ {
					drives[remaining].Health = domain.DriveHealth{Severity: domain.SeverityUnknown, Unsupported: true, Message: "smartctl is not installed"}
				}
				return
			}
			continue
		}
		health, err := parseSMART(result.Stdout, result.ExitCode, state.snapshot.Generated)
		if err != nil {
			state.addError("smart:"+drives[index].ID, "parse", err.Error(), true)
			drives[index].Health = domain.DriveHealth{Severity: domain.SeverityUnknown, Message: "SMART output could not be parsed"}
			continue
		}
		drives[index].Health = health
		if result.ExitCode&0x07 != 0 && !health.Sleeping {
			state.addError("smart:"+drives[index].ID, "incomplete", health.Message, true)
		}
	}
}

// smartReadArgs avoids device-type autodetection, which can itself spin up
// media. ATA's second exit status makes unsupported/unknown power checks stop
// before SMART reads. SCSI lacks that fail-closed option, so SAS/SCSI, bridges,
// and unknown transports require an explicitly authorized waking refresh.
func smartReadArgs(drive domain.Drive, wake bool) ([]string, bool) {
	args := []string{"--json=c", "--all"}
	if wake {
		return append(args, drive.DevNode), true
	}
	switch strings.ToLower(drive.Transport) {
	case "nvme":
		args = append(args, "--device", "nvme")
	case "ata", "sata":
		deviceType := "sat"
		if strings.HasPrefix(drive.DevNode, "/dev/hd") {
			deviceType = "ata"
		}
		args = append(args, "--device", deviceType, "--nocheck", "standby,2,2")
	default:
		return nil, false
	}
	return append(args, drive.DevNode), true
}

func (state *collectionState) markDuplicateIdentities(drives []domain.Drive) {
	indices := make(map[string][]int)
	for index := range drives {
		indices[drives[index].ID] = append(indices[drives[index].ID], index)
	}
	for id, duplicates := range indices {
		if id == "" || len(duplicates) < 2 {
			continue
		}
		for _, index := range duplicates {
			drives[index].Bay.Confidence = "conflict"
			drives[index].ID = "ambiguous:" + strings.TrimPrefix(id, "serial:") + ":" + filepath.Base(drives[index].DevNode)
		}
		state.addError("drive.identity", "ambiguous", fmt.Sprintf("%d drives reported the same identity %q", len(duplicates), id), true)
	}
}

func (state *collectionState) collectARC() {
	data, err := state.collector.fs.ReadFile(state.options.ARCStatsPath)
	if err != nil {
		state.addError("arcstats", classifyFilesystemError(err), err.Error(), true)
		return
	}
	arc, err := parseARCStats(data)
	if err != nil {
		state.addError("arcstats", "parse", err.Error(), true)
		return
	}
	state.snapshot.ARC = arc
}

func (state *collectionState) run(parent context.Context, program string, args ...string) Result {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, state.options.CommandTimeout)
	defer cancel()
	startedAt := state.collector.now()
	wallStart := time.Now()
	command := Command{Program: program, Args: append([]string(nil), args...), Capability: CapabilityObserve, Timeout: state.options.CommandTimeout}
	result := state.collector.runner.Run(ctx, command)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
	}
	if state.options.IncludeCommandTrace {
		state.snapshot.Commands = append(state.snapshot.Commands, domain.CommandTrace{
			Program:      program,
			Args:         append([]string(nil), args...),
			Capability:   CapabilityObserve,
			Risk:         domain.RiskRead,
			StartedAt:    startedAt,
			DurationMS:   time.Since(wallStart).Milliseconds(),
			ExitCode:     result.ExitCode,
			TimedOut:     result.TimedOut,
			Error:        errorText(result.Err),
			OutputHidden: false,
		})
	}
	return result
}

func successful(result Result) bool {
	return result.Err == nil && result.ExitCode == 0 && !result.TimedOut
}

func parseSuccessful[T any](result Result, parser func([]byte) ([]T, error)) ([]T, error) {
	if isNoObjects(result) {
		return []T{}, nil
	}
	if !successful(result) {
		return nil, errors.New(result.ErrString())
	}
	return parser(result.Stdout)
}

func parseSuccessfulMap(result Result, parser func([]byte) (map[string]map[string]string, error)) (map[string]map[string]string, error) {
	if isNoObjects(result) {
		return map[string]map[string]string{}, nil
	}
	if !successful(result) {
		return nil, errors.New(result.ErrString())
	}
	return parser(result.Stdout)
}

func bestFailure(primary, fallback Result) Result {
	if fallback.Err != nil || fallback.ExitCode != 0 {
		return fallback
	}
	return primary
}

func (state *collectionState) addFallbackError(source string, primary Result, parseErr error) {
	kind := classifyError(primary)
	message := primary.ErrString()
	if successful(primary) && parseErr != nil {
		kind = "parse"
		message = parseErr.Error()
	}
	if message == "" {
		message = "JSON capability unavailable"
	}
	state.addError(source+".json", kind, message+"; used tabular compatibility output", true)
}

func (state *collectionState) addCollectionFailure(source string, primary Result, primaryParseErr error, fallback Result, fallbackParseErr error) {
	if !successful(fallback) {
		state.addResultError(source, fallback, true)
		return
	}
	if fallbackParseErr != nil {
		state.addError(source, "parse", fallbackParseErr.Error(), true)
		return
	}
	if primaryParseErr != nil {
		state.addError(source, "parse", primaryParseErr.Error(), true)
	}
}

func (state *collectionState) addResultError(source string, result Result, partial bool) {
	message := result.ErrString()
	if message == "" {
		message = "command failed"
	}
	state.addError(source, classifyError(result), message, partial)
}

func (state *collectionState) addError(source, kind, message string, partial bool) {
	message = strings.TrimSpace(message)
	for _, existing := range state.snapshot.Errors {
		if existing.Source == source && existing.Kind == kind && existing.Message == message {
			return
		}
	}
	state.snapshot.Errors = append(state.snapshot.Errors, domain.SourceError{Source: source, Kind: kind, Message: message, Partial: partial})
}

func parseOSRelease(data []byte) string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if value := values["PRETTY_NAME"]; value != "" {
		return value
	}
	if value := strings.TrimSpace(values["NAME"] + " " + values["VERSION"]); value != "" {
		return value
	}
	return values["ID"]
}

func classifyFilesystemError(err error) string {
	if err == nil {
		return ""
	}
	return classifyError(Result{Err: err})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
