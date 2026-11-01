// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"fmt"
	"sort"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// These bounds are deliberately far above a normal single-host inventory,
// while preventing one valid-sized cache document from expanding into an
// effectively unbounded Prometheus series set or recursive vdev walk.
const (
	maxSnapshotPools       = 1024
	maxSnapshotDatasets    = 65_536
	maxSnapshotDrives      = 8192
	maxSnapshotFindings    = 65_536
	maxSnapshotErrors      = 8192
	maxSnapshotCommands    = 8192
	maxSnapshotVDevs       = 65_536
	maxSnapshotVDevDepth   = 64
	maxSnapshotMapEntries  = 2048
	maxSnapshotCommandArgs = 256
	maxSnapshotStringBytes = 16 << 10
)

func validateSnapshotEnvelope(snapshot domain.Snapshot) error {
	for _, bound := range []struct {
		name  string
		count int
		limit int
	}{
		{"pools", len(snapshot.Pools), maxSnapshotPools},
		{"datasets", len(snapshot.Datasets), maxSnapshotDatasets},
		{"drives", len(snapshot.Drives), maxSnapshotDrives},
		{"findings", len(snapshot.Findings), maxSnapshotFindings},
		{"source_errors", len(snapshot.Errors), maxSnapshotErrors},
		{"command_trace", len(snapshot.Commands), maxSnapshotCommands},
	} {
		if bound.count > bound.limit {
			return fmt.Errorf("snapshot %s count %d exceeds limit %d", bound.name, bound.count, bound.limit)
		}
	}

	if err := validateEnvelopeTexts("host", []namedText{
		{"hostname", snapshot.Host.Hostname}, {"machine_id", snapshot.Host.MachineID},
		{"os", snapshot.Host.OS}, {"kernel", snapshot.Host.Kernel},
		{"openzfs_version", snapshot.Host.OpenZFSVersion}, {"proxmox_version", snapshot.Host.ProxmoxVersion},
		{"proxmox_node", snapshot.Host.ProxmoxNode}, {"proxmox_boot_mode", snapshot.Host.ProxmoxBootMode},
	}); err != nil {
		return err
	}

	vdevCount := 0
	for index, pool := range snapshot.Pools {
		path := fmt.Sprintf("pools[%d]", index)
		if err := validateEnvelopeTexts(path, []namedText{
			{"name", pool.Name}, {"guid", pool.GUID}, {"state", pool.State},
			{"status", pool.Status}, {"action", pool.Action},
			{"scan.function", pool.Scan.Function}, {"scan.state", pool.Scan.State},
			{"trim.state", pool.Trim.State},
		}); err != nil {
			return err
		}
		if err := validateSnapshotVDevs(path+".vdevs", pool.VDevs, 1, &vdevCount); err != nil {
			return err
		}
	}

	for index, dataset := range snapshot.Datasets {
		path := fmt.Sprintf("datasets[%d]", index)
		if err := validateEnvelopeTexts(path, []namedText{
			{"name", dataset.Name}, {"guid", dataset.GUID}, {"type", dataset.Type},
		}); err != nil {
			return err
		}
		if err := validateSnapshotStringMap(path+".properties", dataset.Properties); err != nil {
			return err
		}
		if err := validateSnapshotStringMap(path+".property_sources", dataset.PropertySources); err != nil {
			return err
		}
	}

	for index, drive := range snapshot.Drives {
		path := fmt.Sprintf("drives[%d]", index)
		if err := validateEnvelopeTexts(path, []namedText{
			{"id", drive.ID}, {"devnode", drive.DevNode}, {"major_minor", drive.MajorMinor},
			{"by_id", drive.ByID}, {"sysfs_path", drive.SysfsPath}, {"wwn", drive.WWN},
			{"serial", drive.Serial}, {"vendor", drive.Vendor}, {"model", drive.Model},
			{"firmware", drive.Firmware}, {"transport", drive.Transport}, {"media_type", drive.MediaType},
			{"bay.enclosure", drive.Bay.Enclosure}, {"bay.enclosure_identity", drive.Bay.EnclosureIdentity},
			{"bay.label", drive.Bay.Label}, {"bay.source", drive.Bay.Source},
			{"bay.confidence", drive.Bay.Confidence}, {"health.severity", string(drive.Health.Severity)},
			{"health.message", drive.Health.Message}, {"use", drive.Use},
		}); err != nil {
			return err
		}
	}

	for index, finding := range snapshot.Findings {
		if err := validateEnvelopeTexts(fmt.Sprintf("findings[%d]", index), []namedText{
			{"id", finding.ID}, {"severity", string(finding.Severity)}, {"scope", finding.Scope},
			{"title", finding.Title}, {"evidence", finding.Evidence}, {"remediation", finding.Remediation},
		}); err != nil {
			return err
		}
	}
	for index, sourceError := range snapshot.Errors {
		if err := validateEnvelopeTexts(fmt.Sprintf("source_errors[%d]", index), []namedText{
			{"source", sourceError.Source}, {"kind", sourceError.Kind}, {"message", sourceError.Message},
		}); err != nil {
			return err
		}
	}
	for index, command := range snapshot.Commands {
		path := fmt.Sprintf("command_trace[%d]", index)
		if len(command.Args) > maxSnapshotCommandArgs {
			return fmt.Errorf("snapshot %s.args count %d exceeds limit %d", path, len(command.Args), maxSnapshotCommandArgs)
		}
		if err := validateEnvelopeTexts(path, []namedText{
			{"program", command.Program}, {"capability", command.Capability},
			{"risk", string(command.Risk)}, {"error", command.Error},
		}); err != nil {
			return err
		}
		for argumentIndex, argument := range command.Args {
			if err := validateSnapshotText(fmt.Sprintf("%s.args[%d]", path, argumentIndex), argument); err != nil {
				return err
			}
		}
	}
	return nil
}

type namedText struct {
	name  string
	value string
}

func validateEnvelopeTexts(path string, values []namedText) error {
	for _, value := range values {
		if err := validateSnapshotText(path+"."+value.name, value.value); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshotText(path, value string) error {
	if len(value) > maxSnapshotStringBytes {
		return fmt.Errorf("snapshot %s is %d bytes, exceeding limit %d", path, len(value), maxSnapshotStringBytes)
	}
	return nil
}

func validateSnapshotStringMap(path string, values map[string]string) error {
	if len(values) > maxSnapshotMapEntries {
		return fmt.Errorf("snapshot %s has %d entries, exceeding limit %d", path, len(values), maxSnapshotMapEntries)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		if err := validateSnapshotText(fmt.Sprintf("%s key[%d]", path, index), key); err != nil {
			return err
		}
		if err := validateSnapshotText(fmt.Sprintf("%s value[%d]", path, index), values[key]); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshotVDevs(path string, vdevs []domain.VDev, depth int, total *int) error {
	if len(vdevs) == 0 {
		return nil
	}
	if depth > maxSnapshotVDevDepth {
		return fmt.Errorf("snapshot %s depth %d exceeds limit %d", path, depth, maxSnapshotVDevDepth)
	}
	*total += len(vdevs)
	if *total > maxSnapshotVDevs {
		return fmt.Errorf("snapshot vdev count %d exceeds limit %d", *total, maxSnapshotVDevs)
	}
	for index, vdev := range vdevs {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		if err := validateEnvelopeTexts(itemPath, []namedText{
			{"name", vdev.Name}, {"guid", vdev.GUID}, {"type", vdev.Type}, {"class", vdev.Class},
			{"state", vdev.State}, {"path", vdev.Path}, {"stable_drive_id", vdev.StableDriveID},
			{"trim.state", vdev.Trim.State},
		}); err != nil {
			return err
		}
		if err := validateSnapshotVDevs(itemPath+".children", vdev.Children, depth+1, total); err != nil {
			return err
		}
	}
	return nil
}
