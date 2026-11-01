// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
)

func parseLSBLK(data []byte) ([]domain.Drive, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	rawDevices, present := root["blockdevices"]
	if !present {
		return nil, errors.New("missing blockdevices array")
	}
	devices, ok := rawDevices.([]any)
	if !ok {
		return nil, errors.New("blockdevices is not an array")
	}
	for index, raw := range devices {
		if err := validateLSBLKDevice(raw, fmt.Sprintf("blockdevices[%d]", index)); err != nil {
			return nil, err
		}
	}
	var drives []domain.Drive
	var walk func([]any)
	walk = func(values []any) {
		for _, raw := range values {
			device := object(raw)
			if device == nil {
				continue
			}
			children := array(device["children"])
			if strings.EqualFold(text(device["type"]), "disk") {
				drive := domain.Drive{
					DevNode:    text(first(device, "path", "name")),
					BlockNodes: collectBlockNodes(device),
					MajorMinor: text(first(device, "maj:min", "maj_min", "major_minor")),
					WWN:        normalizeIdentifier(text(device["wwn"])),
					Serial:     strings.TrimSpace(text(device["serial"])),
					Vendor:     strings.TrimSpace(text(device["vendor"])),
					Model:      strings.TrimSpace(text(device["model"])),
					Firmware:   strings.TrimSpace(text(first(device, "rev", "firmware"))),
					Transport:  strings.ToLower(text(first(device, "tran", "transport"))),
					Bay:        domain.Bay{Confidence: "unknown"},
					Health:     domain.DriveHealth{Severity: domain.SeverityUnknown},
				}
				if drive.DevNode != "" && !strings.HasPrefix(drive.DevNode, "/") {
					drive.DevNode = filepath.Join("/dev", drive.DevNode)
				}
				drive.CapacityBytes, _ = unsigned(device["size"])
				drive.LogicalSectorBytes, _ = unsigned(first(device, "log-sec", "log_sec", "logical_sector_size"))
				drive.PhysicalSectorBytes, _ = unsigned(first(device, "phy-sec", "phy_sec", "physical_sector_size"))
				if rotational, ok := boolean(first(device, "rota", "rotational")); ok {
					drive.Rotational = boolPointer(rotational)
					if rotational {
						drive.MediaType = "hdd"
					} else {
						drive.MediaType = "ssd"
					}
				}
				if drive.Transport == "nvme" {
					drive.MediaType = "nvme"
				}
				uses := collectBlockUses(device)
				if len(uses) > 0 {
					drive.InUse = true
					drive.Use = strings.Join(uses, ", ")
				}
				drives = append(drives, drive)
			}
			walk(children)
		}
	}
	walk(devices)
	sort.Slice(drives, func(i, j int) bool { return drives[i].DevNode < drives[j].DevNode })
	return drives, nil
}

// validateLSBLKDevice rejects structurally incomplete device trees before any
// drive can be treated as unused. MOUNTPOINTS and FSTYPE are safety authority:
// silently accepting a missing or wrong-typed value could hide a mounted
// partition or an ownership signature from destructive planning.
func validateLSBLKDevice(raw any, location string) error {
	device, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is not an object", location)
	}
	name := text(first(device, "kname", "path", "name"))
	if name == "" {
		return fmt.Errorf("%s is missing its block-device name", location)
	}
	typeName := text(device["type"])
	if typeName == "" {
		return fmt.Errorf("%s (%s) is missing its device type", location, name)
	}

	rawMountpoints, present := device["mountpoints"]
	if !present {
		return fmt.Errorf("%s (%s) is missing its mountpoints array", location, name)
	}
	mountpoints, ok := rawMountpoints.([]any)
	if !ok {
		return fmt.Errorf("%s (%s) mountpoints is not an array", location, name)
	}
	for index, mountpoint := range mountpoints {
		if mountpoint == nil {
			continue
		}
		if _, ok := mountpoint.(string); !ok {
			return fmt.Errorf("%s (%s) mountpoints[%d] is not text or null", location, name, index)
		}
	}
	if rawMountpoint, present := device["mountpoint"]; present && rawMountpoint != nil {
		if _, ok := rawMountpoint.(string); !ok {
			return fmt.Errorf("%s (%s) mountpoint is not text or null", location, name)
		}
	}
	rawFilesystem, present := device["fstype"]
	if !present {
		return fmt.Errorf("%s (%s) is missing its filesystem type", location, name)
	}
	if rawFilesystem != nil {
		if _, ok := rawFilesystem.(string); !ok {
			return fmt.Errorf("%s (%s) filesystem type is not text or null", location, name)
		}
	}

	rawChildren, present := device["children"]
	if !present {
		return nil
	}
	children, ok := rawChildren.([]any)
	if !ok {
		return fmt.Errorf("%s (%s) children is not an array", location, name)
	}
	for index, child := range children {
		if err := validateLSBLKDevice(child, fmt.Sprintf("%s.children[%d]", location, index)); err != nil {
			return err
		}
	}
	return nil
}

func collectBlockNodes(device map[string]any) []string {
	seen := make(map[string]bool)
	var nodes []string
	var walk func(map[string]any)
	walk = func(current map[string]any) {
		name := filepath.Base(filepath.Clean(text(first(current, "kname", "path", "name"))))
		if name != "" && name != "." && name != string(filepath.Separator) && !seen[name] {
			seen[name] = true
			nodes = append(nodes, name)
		}
		for _, rawChild := range array(current["children"]) {
			if child := object(rawChild); child != nil {
				walk(child)
			}
		}
	}
	walk(device)
	sort.Strings(nodes)
	return nodes
}

func collectBlockUses(device map[string]any) []string {
	seen := make(map[string]bool)
	var result []string
	var walk func(map[string]any, bool)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "-" || seen[value] {
			return
		}
		seen[value] = true
		result = append(result, value)
	}
	walk = func(current map[string]any, root bool) {
		name := text(first(current, "path", "name"))
		if name != "" && !strings.HasPrefix(name, "/") {
			name = filepath.Join("/dev", name)
		}
		if mountpoints := array(current["mountpoints"]); mountpoints != nil {
			for _, rawMountpoint := range mountpoints {
				if mountpoint := text(rawMountpoint); mountpoint != "" {
					add(fmt.Sprintf("%s mounted at %s", name, mountpoint))
				}
			}
		} else if mountpoint := text(current["mountpoint"]); mountpoint != "" {
			add(fmt.Sprintf("%s mounted at %s", name, mountpoint))
		}
		if filesystem := text(current["fstype"]); filesystem != "" {
			add(fmt.Sprintf("%s contains %s", name, filesystem))
		}
		typeName := strings.ToLower(text(current["type"]))
		// A partition is an inactive on-disk layout by itself. Every other
		// descendant block-device type represents an active holder/mapping and
		// must prevent disk preparation (crypt, dm, mpath, LVM, MD, loop, ...).
		if !root && typeName != "part" && typeName != "" {
			add(fmt.Sprintf("%s is held by %s", name, typeName))
		} else if !root && typeName == "" {
			add(fmt.Sprintf("%s is held by an unknown block mapping", name))
		}
		for _, rawChild := range array(current["children"]) {
			if child := object(rawChild); child != nil {
				walk(child, false)
			}
		}
	}
	walk(device, true)
	sort.Strings(result)
	return result
}

func boolPointer(value bool) *bool { return &value }

func parseUdevProperties(data []byte) map[string]string {
	result := make(map[string]string)
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			result[key] = strings.TrimSpace(value)
		}
	}
	return result
}

func enrichDriveFromUdev(drive *domain.Drive, properties map[string]string) {
	if value := firstNonEmpty(properties, "ID_WWN_WITH_EXTENSION", "ID_WWN", "ID_NVME_EUI64", "ID_NVME_NGUID"); value != "" {
		drive.WWN = normalizeIdentifier(value)
	}
	if value := firstNonEmpty(properties, "ID_SERIAL_SHORT", "ID_SCSI_SERIAL"); value != "" {
		drive.Serial = value
	}
	if value := properties["ID_VENDOR"]; value != "" {
		drive.Vendor = decodeUdevEscapes(value)
	}
	if value := properties["ID_MODEL"]; value != "" {
		drive.Model = decodeUdevEscapes(value)
	}
	if value := properties["ID_REVISION"]; value != "" {
		drive.Firmware = value
	}
	if value := properties["ID_BUS"]; value != "" {
		drive.Transport = strings.ToLower(value)
	}
	if value := properties["DEVPATH"]; value != "" {
		drive.SysfsPath = filepath.Join("/sys", strings.TrimPrefix(value, "/"))
	}
	if properties["ID_ATA_ROTATION_RATE_RPM"] == "0" || properties["ID_DRIVE_FLASH_SD"] == "1" {
		drive.Rotational = boolPointer(false)
		drive.MediaType = "ssd"
	}
	if strings.EqualFold(drive.Transport, "nvme") {
		drive.MediaType = "nvme"
	}
}

func firstNonEmpty(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func decodeUdevEscapes(value string) string {
	// udev encodes spaces as \x20 in ID_MODEL/ID_VENDOR.  strconv.Unquote
	// decodes all of its documented hexadecimal byte escapes.
	if decoded, err := strconv.Unquote(`"` + strings.ReplaceAll(value, `"`, `\"`) + `"`); err == nil {
		return strings.TrimSpace(decoded)
	}
	return strings.TrimSpace(strings.ReplaceAll(value, `\x20`, " "))
}

var partitionLinkPattern = regexp.MustCompile(`-part[0-9]+$`)

func applyByID(fsys FileSystem, directory string, drives []domain.Drive) error {
	entries, err := fsys.ReadDir(directory)
	if err != nil {
		return err
	}
	type candidate struct {
		path string
		rank int
	}
	byTarget := make(map[string][]candidate)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || partitionLinkPattern.MatchString(name) {
			continue
		}
		if strings.HasPrefix(name, "wwn-") {
			if _, ok := inventory.CanonicalDriveWWN(strings.TrimPrefix(name, "wwn-")); !ok {
				continue
			}
		}
		if strings.HasPrefix(name, "nvme-eui.") {
			if _, ok := inventory.CanonicalDriveWWN(strings.TrimPrefix(name, "nvme-")); !ok {
				continue
			}
		}
		path := filepath.Join(directory, name)
		target, err := fsys.EvalSymlinks(path)
		if err != nil {
			continue
		}
		byTarget[filepath.Clean(target)] = append(byTarget[filepath.Clean(target)], candidate{path: path, rank: byIDRank(name)})
	}
	for index := range drives {
		candidates := byTarget[filepath.Clean(drives[index].DevNode)]
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].rank != candidates[j].rank {
				return candidates[i].rank < candidates[j].rank
			}
			return candidates[i].path < candidates[j].path
		})
		drives[index].ByID = candidates[0].path
		name := filepath.Base(candidates[0].path)
		if drives[index].WWN == "" && (strings.HasPrefix(name, "wwn-") || strings.HasPrefix(name, "nvme-eui.")) {
			drives[index].WWN = normalizeIdentifier(strings.TrimPrefix(strings.TrimPrefix(name, "wwn-"), "nvme-"))
		}
	}
	return nil
}

func applyByPath(fsys FileSystem, directory string, drives []domain.Drive) error {
	entries, err := fsys.ReadDir(directory)
	if err != nil {
		return err
	}
	byTarget := make(map[string][]string)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || partitionLinkPattern.MatchString(name) {
			continue
		}
		path := filepath.Join(directory, name)
		target, err := fsys.EvalSymlinks(path)
		if err != nil {
			continue
		}
		byTarget[filepath.Clean(target)] = append(byTarget[filepath.Clean(target)], name)
	}
	for index := range drives {
		candidates := byTarget[filepath.Clean(drives[index].DevNode)]
		if len(candidates) == 0 || drives[index].Bay.Confidence == "verified" {
			continue
		}
		sort.Strings(candidates)
		drives[index].Bay = domain.Bay{
			Enclosure: "linux-by-path", Label: candidates[0], Source: "by-path", Confidence: "inferred",
		}
	}
	return nil
}

func byIDRank(name string) int {
	switch {
	case strings.HasPrefix(name, "wwn-"):
		return 0
	case strings.HasPrefix(name, "nvme-eui."), strings.HasPrefix(name, "nvme-uuid."):
		return 1
	case strings.HasPrefix(name, "nvme-"):
		return 2
	case strings.HasPrefix(name, "ata-"):
		return 3
	case strings.HasPrefix(name, "scsi-"):
		return 4
	default:
		return 9
	}
}

func normalizeIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "0x")
	return value
}

func finalizeDriveIdentity(drive *domain.Drive) {
	drive.ID = ""
	if canonical, ok := inventory.CanonicalDriveWWN(drive.WWN); ok {
		drive.WWN = canonical
		drive.ID = "wwn:" + canonical
		return
	}
	drive.WWN = ""
	if canonical, ok := inventory.CanonicalSESSerial(drive.Serial); ok &&
		!strings.Contains(drive.Serial, "|") && !strings.Contains(drive.Model, "|") {
		drive.Serial = strings.TrimPrefix(canonical, "scsi-serial:")
		identity := strings.Join([]string{drive.Serial, drive.Model, strconv.FormatUint(drive.CapacityBytes, 10)}, "|")
		drive.ID = "serial:" + strings.ToLower(identity)
		return
	}
	drive.Serial = ""
	drive.ID = "path:" + drive.DevNode
}

// parseSMART handles smartctl's common ATA, SCSI/SAS, and NVMe JSON shapes.
// A non-zero exit code is intentionally not an error: smartctl encodes health
// findings in a bitmask and still emits valid, useful JSON.
func parseSMART(data []byte, exitCode int, now time.Time) (domain.DriveHealth, error) {
	if exitCode < 0 || exitCode > 255 {
		return domain.DriveHealth{}, fmt.Errorf("invalid smartctl exit bitmask %d", exitCode)
	}
	root, err := decodeObject(data)
	if err != nil {
		return domain.DriveHealth{}, err
	}
	health := domain.DriveHealth{Severity: domain.SeverityUnknown}
	if smartctl := object(root["smartctl"]); smartctl != nil {
		if rawStatus, present := smartctl["exit_status"]; present {
			embedded, err := strconv.ParseUint(text(rawStatus), 10, 8)
			if err != nil {
				return domain.DriveHealth{}, errors.New("invalid embedded smartctl exit bitmask")
			}
			// Neither status source may erase a failure reported by the other.
			exitCode |= int(embedded)
		}
	}
	powerModeValue := first(root, "power_mode", "power-mode")
	if mode := object(powerModeValue); mode != nil {
		powerModeValue = mode["name"]
	}
	if powerMode := strings.ToLower(text(powerModeValue)); strings.Contains(powerMode, "standby") || strings.Contains(powerMode, "sleep") {
		health.Sleeping = true
		health.Severity = domain.SeverityUnknown
		health.Message = "drive is in " + powerMode + "; SMART data was not refreshed"
		return health, nil
	}

	if smartStatus := object(root["smart_status"]); smartStatus != nil {
		if passed, ok := boolean(smartStatus["passed"]); ok {
			health.SMARTPassed = boolPointer(passed)
			if !passed {
				health.Severity = domain.SeverityCritical
				health.Message = "SMART overall-health self-assessment failed"
			}
		}
	}
	if temperature := object(root["temperature"]); temperature != nil {
		if value, ok := decimal(first(temperature, "current", "temperature")); ok {
			health.TemperatureC = floatPointer(value)
		}
	}
	if powerOn := object(root["power_on_time"]); powerOn != nil {
		if value, ok := unsigned(first(powerOn, "hours", "hour")); ok {
			health.PowerOnHours = uintPointer(value)
		}
	}

	if nvme := object(root["nvme_smart_health_information_log"]); nvme != nil {
		if health.TemperatureC == nil {
			if value, ok := decimal(first(nvme, "temperature", "temperature_sensor_1")); ok {
				health.TemperatureC = floatPointer(value)
			}
		}
		// NVMe reports an unsigned byte: 100 means rated endurance is used,
		// and values through 255 remain valid after that point.
		if value, err := strconv.ParseUint(text(nvme["percentage_used"]), 10, 8); err == nil {
			health.PercentageUsed = floatPointer(float64(value))
		}
		if value, ok := unsigned(first(nvme, "media_errors", "media_and_data_integrity_errors")); ok {
			health.MediaErrors = uintPointer(value)
		}
		if health.PowerOnHours == nil {
			if value, ok := unsigned(nvme["power_on_hours"]); ok {
				health.PowerOnHours = uintPointer(value)
			}
		}
	}

	parseATAAttributes(root, &health)
	parseSCSIHealth(root, &health)

	// A syntactically valid smartctl JSON object is not necessarily a health
	// sample.  In particular, empty/unsupported responses must never become a
	// fresh green result.  Health/history exit bits are themselves recognized
	// evidence, even when a device exposes no normalized attributes.
	recognized := health.SMARTPassed != nil || health.TemperatureC != nil ||
		health.PowerOnHours != nil || health.PercentageUsed != nil ||
		health.MediaErrors != nil || health.Reallocated != nil ||
		health.Pending != nil || health.OfflineUncorrect != nil ||
		health.CRCErrorCount != nil || exitCode&0xf8 != 0
	if recognized {
		health.FreshAt = now
		if health.Severity == domain.SeverityUnknown {
			health.Severity = domain.SeverityOK
		}
	} else {
		health.Message = "SMART JSON contained no recognized health data"
	}

	// smartctl exit bits 3 and 4 indicate a failing overall assessment or a
	// currently failing pre-fail attribute.  Bits 5-7 retain important health
	// history and therefore warrant a warning even if normalized counters were
	// not present in this particular device's JSON shape.
	if exitCode&0x18 != 0 {
		health.Severity = domain.SeverityCritical
		if health.Message == "" {
			health.Message = fmt.Sprintf("smartctl reported failing health (exit bitmask %d)", exitCode)
		}
	} else if exitCode&0xe0 != 0 && health.Severity != domain.SeverityCritical {
		health.Severity = domain.SeverityWarning
		if health.Message == "" {
			health.Message = fmt.Sprintf("smartctl reported health history warnings (exit bitmask %d)", exitCode)
		}
	}

	// Bits 0-2 mean invocation, device-open, or command failure.  With valid
	// JSON we preserve recognized parsed data but mark it incomplete.
	if exitCode&0x07 != 0 {
		if health.Severity != domain.SeverityCritical {
			health.Severity = domain.SeverityUnknown
		}
		if health.Message == "" {
			health.Message = fmt.Sprintf("smartctl reported incomplete data (exit bitmask %d)", exitCode)
		}
	}
	return health, nil
}

func parseATAAttributes(root map[string]any, health *domain.DriveHealth) {
	attributes := object(root["ata_smart_attributes"])
	if attributes == nil {
		return
	}
	var wearCandidate *float64
	wearConflict := false
	for _, raw := range array(attributes["table"]) {
		attribute := object(raw)
		if attribute == nil {
			continue
		}
		idValue, _ := unsigned(attribute["id"])
		name := normalizeSMARTAttributeName(text(attribute["name"]))
		if health.PercentageUsed == nil {
			if used, ok := ataPercentageUsed(attribute, name); ok {
				if wearCandidate == nil {
					wearCandidate = floatPointer(used)
				} else if *wearCandidate != used {
					// Multiple vendor attributes which disagree are not a safe
					// basis for claiming a precise wear percentage.
					wearConflict = true
				}
			}
		}
		rawObject := object(attribute["raw"])
		value, ok := unsigned(first(rawObject, "value", "string"))
		if !ok {
			continue
		}
		switch {
		case idValue == 5 || strings.Contains(name, "reallocated_sector"):
			health.Reallocated = uintPointer(value)
		case idValue == 197 || strings.Contains(name, "current_pending"):
			health.Pending = uintPointer(value)
		case idValue == 198 || strings.Contains(name, "offline_uncorrect"):
			health.OfflineUncorrect = uintPointer(value)
		case idValue == 199 || strings.Contains(name, "udma_crc"):
			health.CRCErrorCount = uintPointer(value)
		case (idValue == 9 || strings.Contains(name, "power_on_hours")) && health.PowerOnHours == nil:
			health.PowerOnHours = uintPointer(value)
		}
	}
	if health.PercentageUsed == nil && wearCandidate != nil && !wearConflict {
		health.PercentageUsed = wearCandidate
	}
}

func ataPercentageUsed(attribute map[string]any, name string) (float64, bool) {
	value, ok := percentage(decimal(attribute["value"]))
	if !ok {
		return 0, false
	}
	switch name {
	case "media_wearout_indicator", "percent_lifetime_remain", "ssd_life_left", "remaining_lifetime":
		return 100 - value, true
	case "percentage_used", "percent_lifetime_used", "percentage_lifetime_used":
		return value, true
	default:
		return 0, false
	}
}

func percentage(value float64, ok bool) (float64, bool) {
	return value, ok && value >= 0 && value <= 100 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func normalizeSMARTAttributeName(value string) string {
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(value)), func(character rune) bool {
		return !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9'))
	})
	return strings.Join(fields, "_")
}

func parseSCSIHealth(root map[string]any, health *domain.DriveHealth) {
	log := object(root["scsi_error_counter_log"])
	if log == nil {
		return
	}
	var total uint64
	found := false
	for _, operation := range []string{"read", "write", "verify"} {
		entry := object(log[operation])
		if value, ok := unsigned(entry["total_uncorrected_errors"]); ok {
			total = saturatingAdd(total, value)
			found = true
		}
	}
	if found {
		health.MediaErrors = uintPointer(total)
	}
}

func floatPointer(value float64) *float64 { return &value }
func uintPointer(value uint64) *uint64    { return &value }

func correlateVDevs(fsys FileSystem, pools []domain.Pool, drives []domain.Drive) {
	byPath := make(map[string]int)
	for index := range drives {
		byPath[filepath.Clean(drives[index].DevNode)] = index
		if drives[index].ByID != "" {
			byPath[filepath.Clean(drives[index].ByID)] = index
		}
	}
	for poolIndex := range pools {
		var walk func([]domain.VDev)
		walk = func(vdevs []domain.VDev) {
			for vdevIndex := range vdevs {
				vdev := &vdevs[vdevIndex]
				if len(vdev.Children) > 0 {
					walk(vdev.Children)
					continue
				}
				index, ok := matchDrivePath(fsys, vdev.Path, drives, byPath)
				if !ok {
					continue
				}
				vdev.StableDriveID = drives[index].ID
				drives[index].InUse = true
				usage := "ZFS pool " + pools[poolIndex].Name
				if drives[index].Use == "" {
					drives[index].Use = usage
				} else if !strings.Contains(drives[index].Use, usage) {
					drives[index].Use += ", " + usage
				}
			}
		}
		walk(pools[poolIndex].VDevs)
	}
}

func matchDrivePath(fsys FileSystem, path string, drives []domain.Drive, byPath map[string]int) (int, bool) {
	if path == "" {
		return 0, false
	}
	clean := filepath.Clean(path)
	if index, ok := byPath[clean]; ok {
		return index, true
	}
	resolvedPath := false
	if resolved, err := fsys.EvalSymlinks(clean); err == nil {
		resolvedPath = true
		clean = filepath.Clean(resolved)
		if index, ok := byPath[clean]; ok {
			return index, true
		}
	}
	for index := range drives {
		if isPartitionOf(clean, drives[index].DevNode) ||
			(!resolvedPath && drives[index].ByID != "" && strings.HasPrefix(path, drives[index].ByID+"-part") &&
				validPartitionNumber(strings.TrimPrefix(path, drives[index].ByID+"-part"))) {
			return index, true
		}
	}
	return 0, false
}

func isPartitionOf(partition, disk string) bool {
	partition = filepath.Clean(partition)
	disk = filepath.Clean(disk)
	if !strings.HasPrefix(partition, disk) || partition == disk {
		return false
	}
	suffix := strings.TrimPrefix(partition, disk)
	if strings.HasPrefix(filepath.Base(disk), "nvme") || strings.HasPrefix(filepath.Base(disk), "mmcblk") {
		if !strings.HasPrefix(suffix, "p") {
			return false
		}
		suffix = strings.TrimPrefix(suffix, "p")
	}
	return validPartitionNumber(suffix)
}

func validPartitionNumber(value string) bool {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(value, 10, 32)
	return err == nil
}
