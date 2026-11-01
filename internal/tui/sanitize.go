// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"sort"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// sanitizeTerminalText makes collector- and cache-derived text inert before it
// is mixed with the TUI's own ANSI styling. Replacing controls, rather than
// silently deleting them, also leaves a visible indication that the displayed
// identity was unusual.
func sanitizeTerminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if isUnsafeTerminalRune(r) {
			return '\uFFFD'
		}
		return r
	}, value)
}

func isUnsafeTerminalRune(r rune) bool {
	if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
		return true
	}
	// Directional marks, embeddings, overrides, isolates, and the deprecated
	// directional formatting controls can visually reorder trusted labels.
	switch r {
	case '\u061c', '\u200e', '\u200f':
		return true
	}
	return (r >= '\u202a' && r <= '\u202e') ||
		(r >= '\u2066' && r <= '\u206f')
}

// sanitizeSnapshot returns an independently backed copy so rendering never
// mutates a caller's cached snapshot while neutralizing every string that can
// reach the terminal.
func sanitizeSnapshot(in domain.Snapshot) domain.Snapshot {
	out := in
	out.APIVersion = sanitizeTerminalText(in.APIVersion)
	out.Host = sanitizeHost(in.Host)

	out.Pools = make([]domain.Pool, len(in.Pools))
	for i, pool := range in.Pools {
		pool.Name = sanitizeTerminalText(pool.Name)
		pool.GUID = sanitizeTerminalText(pool.GUID)
		pool.State = sanitizeTerminalText(pool.State)
		pool.Status = sanitizeTerminalText(pool.Status)
		pool.Action = sanitizeTerminalText(pool.Action)
		pool.Scan.Function = sanitizeTerminalText(pool.Scan.Function)
		pool.Scan.State = sanitizeTerminalText(pool.Scan.State)
		pool.Trim.State = sanitizeTerminalText(pool.Trim.State)
		pool.VDevs = sanitizeVDevs(pool.VDevs)
		out.Pools[i] = pool
	}

	out.Datasets = make([]domain.Dataset, len(in.Datasets))
	for i, dataset := range in.Datasets {
		dataset.Name = sanitizeTerminalText(dataset.Name)
		dataset.GUID = sanitizeTerminalText(dataset.GUID)
		dataset.Type = sanitizeTerminalText(dataset.Type)
		dataset.Properties = sanitizeStringMap(dataset.Properties)
		dataset.PropertySources = sanitizeStringMap(dataset.PropertySources)
		out.Datasets[i] = dataset
	}

	out.Drives = make([]domain.Drive, len(in.Drives))
	for i, drive := range in.Drives {
		drive.ID = sanitizeTerminalText(drive.ID)
		drive.DevNode = sanitizeTerminalText(drive.DevNode)
		drive.MajorMinor = sanitizeTerminalText(drive.MajorMinor)
		drive.ByID = sanitizeTerminalText(drive.ByID)
		drive.SysfsPath = sanitizeTerminalText(drive.SysfsPath)
		drive.WWN = sanitizeTerminalText(drive.WWN)
		drive.Serial = sanitizeTerminalText(drive.Serial)
		drive.Vendor = sanitizeTerminalText(drive.Vendor)
		drive.Model = sanitizeTerminalText(drive.Model)
		drive.Firmware = sanitizeTerminalText(drive.Firmware)
		drive.Transport = sanitizeTerminalText(drive.Transport)
		drive.MediaType = sanitizeTerminalText(drive.MediaType)
		drive.Use = sanitizeTerminalText(drive.Use)
		drive.Bay.Enclosure = sanitizeTerminalText(drive.Bay.Enclosure)
		drive.Bay.EnclosureIdentity = sanitizeTerminalText(drive.Bay.EnclosureIdentity)
		drive.Bay.Label = sanitizeTerminalText(drive.Bay.Label)
		drive.Bay.Source = sanitizeTerminalText(drive.Bay.Source)
		drive.Bay.Confidence = sanitizeTerminalText(drive.Bay.Confidence)
		drive.Health.Severity = domain.Severity(sanitizeTerminalText(string(drive.Health.Severity)))
		drive.Health.Message = sanitizeTerminalText(drive.Health.Message)
		out.Drives[i] = drive
	}

	out.Findings = make([]domain.Finding, len(in.Findings))
	for i, finding := range in.Findings {
		finding.ID = sanitizeTerminalText(finding.ID)
		finding.Severity = domain.Severity(sanitizeTerminalText(string(finding.Severity)))
		finding.Scope = sanitizeTerminalText(finding.Scope)
		finding.Title = sanitizeTerminalText(finding.Title)
		finding.Evidence = sanitizeTerminalText(finding.Evidence)
		finding.Remediation = sanitizeTerminalText(finding.Remediation)
		out.Findings[i] = finding
	}

	out.Errors = make([]domain.SourceError, len(in.Errors))
	for i, sourceErr := range in.Errors {
		sourceErr.Source = sanitizeTerminalText(sourceErr.Source)
		sourceErr.Kind = sanitizeTerminalText(sourceErr.Kind)
		sourceErr.Message = sanitizeTerminalText(sourceErr.Message)
		out.Errors[i] = sourceErr
	}

	out.Commands = make([]domain.CommandTrace, len(in.Commands))
	for i, command := range in.Commands {
		command.Program = sanitizeTerminalText(command.Program)
		command.Args = sanitizeStrings(command.Args)
		command.Capability = sanitizeTerminalText(command.Capability)
		command.Risk = domain.Risk(sanitizeTerminalText(string(command.Risk)))
		command.Error = sanitizeTerminalText(command.Error)
		out.Commands[i] = command
	}
	return out
}

func sanitizeHost(in domain.Host) domain.Host {
	in.Hostname = sanitizeTerminalText(in.Hostname)
	in.MachineID = sanitizeTerminalText(in.MachineID)
	in.OS = sanitizeTerminalText(in.OS)
	in.Kernel = sanitizeTerminalText(in.Kernel)
	in.OpenZFSVersion = sanitizeTerminalText(in.OpenZFSVersion)
	in.ProxmoxVersion = sanitizeTerminalText(in.ProxmoxVersion)
	in.ProxmoxNode = sanitizeTerminalText(in.ProxmoxNode)
	in.ProxmoxBootMode = sanitizeTerminalText(in.ProxmoxBootMode)
	return in
}

func sanitizeVDevs(in []domain.VDev) []domain.VDev {
	out := make([]domain.VDev, len(in))
	for i, vdev := range in {
		vdev.Name = sanitizeTerminalText(vdev.Name)
		vdev.GUID = sanitizeTerminalText(vdev.GUID)
		vdev.Type = sanitizeTerminalText(vdev.Type)
		vdev.Class = sanitizeTerminalText(vdev.Class)
		vdev.State = sanitizeTerminalText(vdev.State)
		vdev.Path = sanitizeTerminalText(vdev.Path)
		vdev.StableDriveID = sanitizeTerminalText(vdev.StableDriveID)
		vdev.Trim.State = sanitizeTerminalText(vdev.Trim.State)
		vdev.Children = sanitizeVDevs(vdev.Children)
		out[i] = vdev
	}
	return out
}

func sanitizeStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, value := range in {
		out[i] = sanitizeTerminalText(value)
	}
	return out
}

func sanitizeStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out[sanitizeTerminalText(key)] = sanitizeTerminalText(in[key])
	}
	return out
}
