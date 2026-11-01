// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// EvaluateFindings applies the configured point-in-time warning thresholds.
// It intentionally does not predict failures from vendor-specific SMART data.
func EvaluateFindings(snapshot domain.Snapshot, thresholds Thresholds) []domain.Finding {
	thresholds = normalizeThresholds(thresholds)
	var findings []domain.Finding
	for _, pool := range snapshot.Pools {
		scope := "pool/" + pool.Name
		if state := normalizedState(pool.State); state != "" && state != "ONLINE" {
			findings = append(findings, domain.Finding{
				ID:          "pool:" + pool.Name + ":state",
				Severity:    domain.SeverityCritical,
				Scope:       scope,
				Title:       fmt.Sprintf("Pool %s is %s", pool.Name, state),
				Evidence:    pool.Status,
				Remediation: "Inspect pool status and restore redundancy before making storage changes.",
			})
		}
		if pool.CapacityPercent >= thresholds.CapacityCriticalPercent {
			findings = append(findings, capacityFinding(pool, domain.SeverityCritical, scope))
		} else if pool.CapacityPercent >= thresholds.CapacityWarningPercent {
			findings = append(findings, capacityFinding(pool, domain.SeverityWarning, scope))
		}
		if pool.FragmentationPct >= thresholds.FragmentationAdvisoryPercent {
			findings = append(findings, domain.Finding{
				ID:       "pool:" + pool.Name + ":fragmentation",
				Severity: domain.SeverityInfo,
				Scope:    scope,
				Title:    fmt.Sprintf("Pool allocator fragmentation is %.1f%%", pool.FragmentationPct),
				Evidence: "This is an allocator metric, not evidence of physical disk damage.",
			})
		}
		if pool.ReadErrors+pool.WriteErrors+pool.ChecksumErrors > 0 {
			findings = append(findings, domain.Finding{
				ID:          "pool:" + pool.Name + ":errors",
				Severity:    domain.SeverityWarning,
				Scope:       scope,
				Title:       "Pool has recorded I/O errors",
				Evidence:    fmt.Sprintf("read=%d write=%d checksum=%d", pool.ReadErrors, pool.WriteErrors, pool.ChecksumErrors),
				Remediation: "Review zpool status, SMART history, and the most recent scrub result.",
			})
		}
		if pool.Trim.Errors > 0 {
			findings = append(findings, domain.Finding{
				ID:          "pool:" + pool.Name + ":trim-errors",
				Severity:    domain.SeverityWarning,
				Scope:       scope,
				Title:       "On-demand TRIM reported errors",
				Evidence:    fmt.Sprintf("state=%s errors=%d", valueOrUnknown(pool.Trim.State), pool.Trim.Errors),
				Remediation: "Inspect per-vdev TRIM status and the storage path before starting another pass.",
			})
		}
	}

	for _, drive := range snapshot.Drives {
		label := driveLabel(drive)
		scope := "drive/" + drive.ID
		if drive.Health.SMARTPassed != nil && !*drive.Health.SMARTPassed {
			findings = append(findings, domain.Finding{
				ID:          "drive:" + drive.ID + ":smart",
				Severity:    domain.SeverityCritical,
				Scope:       scope,
				Title:       label + " failed its SMART health assessment",
				Evidence:    drive.Health.Message,
				Remediation: "Confirm the bay and stable identity, preserve redundancy, and plan replacement.",
			})
		}
		if drive.Health.PercentageUsed != nil {
			used := *drive.Health.PercentageUsed
			switch {
			case used >= thresholds.WearCriticalPercent:
				findings = append(findings, wearFinding(drive, label, used, domain.SeverityCritical, scope))
			case used >= thresholds.WearWarningPercent:
				findings = append(findings, wearFinding(drive, label, used, domain.SeverityWarning, scope))
			}
		}
		if drive.Health.TemperatureC != nil {
			temperature := *drive.Health.TemperatureC
			switch {
			case temperature >= thresholds.TemperatureCriticalC:
				findings = append(findings, temperatureFinding(drive, label, temperature, domain.SeverityCritical, scope))
			case temperature >= thresholds.TemperatureWarningC:
				findings = append(findings, temperatureFinding(drive, label, temperature, domain.SeverityWarning, scope))
			}
		}
		if count := pointerValue(drive.Health.Reallocated); count > 0 {
			findings = append(findings, domain.Finding{
				ID: "drive:" + drive.ID + ":reallocated-sectors", Severity: domain.SeverityWarning, Scope: scope,
				Title: label + " has reallocated sectors", Evidence: fmt.Sprintf("reallocated sectors=%d", count),
				Remediation: "Compare the counter over time, verify backups and redundancy, and consider replacement.",
			})
		}
		if count := pointerValue(drive.Health.Pending); count > 0 {
			findings = append(findings, domain.Finding{
				ID:          "drive:" + drive.ID + ":pending-sectors",
				Severity:    domain.SeverityWarning,
				Scope:       scope,
				Title:       label + " has pending sectors",
				Evidence:    fmt.Sprintf("pending sectors=%d", count),
				Remediation: "Check redundancy and run an appropriate long SMART test or replace the drive.",
			})
		}
		if count := pointerValue(drive.Health.MediaErrors); count > 0 {
			findings = append(findings, domain.Finding{
				ID:          "drive:" + drive.ID + ":media-errors",
				Severity:    domain.SeverityWarning,
				Scope:       scope,
				Title:       label + " reports media errors",
				Evidence:    fmt.Sprintf("media errors=%d", count),
				Remediation: "Compare the counter over time and inspect pool error and scrub history.",
			})
		}
		if count := pointerValue(drive.Health.OfflineUncorrect); count > 0 {
			findings = append(findings, domain.Finding{
				ID: "drive:" + drive.ID + ":offline-uncorrectable", Severity: domain.SeverityCritical, Scope: scope,
				Title: label + " has offline-uncorrectable sectors", Evidence: fmt.Sprintf("offline uncorrectable sectors=%d", count),
				Remediation: "Verify pool redundancy and backups, then plan replacement of this exact drive.",
			})
		}
		if count := pointerValue(drive.Health.CRCErrorCount); count > 0 {
			findings = append(findings, domain.Finding{
				ID: "drive:" + drive.ID + ":crc-errors", Severity: domain.SeverityInfo, Scope: scope,
				Title: label + " has interface CRC errors", Evidence: fmt.Sprintf("CRC errors=%d (the counter is cumulative)", count),
				Remediation: "Watch for increases and inspect the cable, connector, backplane, and HBA path.",
			})
		}
		if drive.Bay.Label == "" {
			findings = append(findings, domain.Finding{
				ID: "drive:" + drive.ID + ":bay-unknown", Severity: domain.SeverityInfo, Scope: scope,
				Title:       label + " has no physical bay mapping",
				Remediation: "Use `zpoolpilot inventory map` and physically verify the label before a replacement.",
			})
		}
		if drive.Health.Sleeping {
			findings = append(findings, domain.Finding{
				ID: "drive:" + drive.ID + ":sleeping", Severity: domain.SeverityInfo, Scope: scope,
				Title:       label + " remained in standby; SMART was not refreshed",
				Remediation: "This is expected in no-wake mode; use a deliberate deep scan if fresh SMART data is required.",
			})
		}
	}

	if snapshot.Partial {
		kinds := make(map[string]bool)
		for _, sourceError := range snapshot.Errors {
			kinds[sourceError.Kind] = true
		}
		kindNames := make([]string, 0, len(kinds))
		for kind := range kinds {
			kindNames = append(kindNames, kind)
		}
		sort.Strings(kindNames)
		findings = append(findings, domain.Finding{
			ID:       "collection:partial",
			Severity: domain.SeverityUnknown,
			Scope:    "host",
			Title:    "Monitoring snapshot is incomplete",
			Evidence: strings.Join(kindNames, ", "),
		})
	}

	sort.SliceStable(findings, func(i, j int) bool {
		left, right := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if left != right {
			return left < right
		}
		return findings[i].ID < findings[j].ID
	})
	return findings
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func normalizeThresholds(value Thresholds) Thresholds {
	defaults := DefaultThresholds()
	if value.CapacityWarningPercent <= 0 {
		value.CapacityWarningPercent = defaults.CapacityWarningPercent
	}
	if value.CapacityCriticalPercent <= 0 {
		value.CapacityCriticalPercent = defaults.CapacityCriticalPercent
	}
	if value.CapacityCriticalPercent < value.CapacityWarningPercent {
		value.CapacityCriticalPercent = value.CapacityWarningPercent
	}
	if value.WearWarningPercent <= 0 {
		value.WearWarningPercent = defaults.WearWarningPercent
	}
	if value.WearCriticalPercent <= 0 {
		value.WearCriticalPercent = defaults.WearCriticalPercent
	}
	if value.WearCriticalPercent < value.WearWarningPercent {
		value.WearCriticalPercent = value.WearWarningPercent
	}
	if value.TemperatureWarningC <= 0 {
		value.TemperatureWarningC = defaults.TemperatureWarningC
	}
	if value.TemperatureCriticalC <= 0 {
		value.TemperatureCriticalC = defaults.TemperatureCriticalC
	}
	if value.TemperatureCriticalC < value.TemperatureWarningC {
		value.TemperatureCriticalC = value.TemperatureWarningC
	}
	if value.FragmentationAdvisoryPercent <= 0 {
		value.FragmentationAdvisoryPercent = defaults.FragmentationAdvisoryPercent
	}
	return value
}

func capacityFinding(pool domain.Pool, severity domain.Severity, scope string) domain.Finding {
	return domain.Finding{
		ID:          "pool:" + pool.Name + ":capacity",
		Severity:    severity,
		Scope:       scope,
		Title:       fmt.Sprintf("Pool is %.1f%% allocated", pool.CapacityPercent),
		Evidence:    fmt.Sprintf("allocated=%d free=%d size=%d bytes", pool.AllocatedBytes, pool.FreeBytes, pool.SizeBytes),
		Remediation: "Free space or add matching redundant capacity before the pool fills.",
	}
}

func wearFinding(drive domain.Drive, label string, used float64, severity domain.Severity, scope string) domain.Finding {
	return domain.Finding{
		ID:          "drive:" + drive.ID + ":wear",
		Severity:    severity,
		Scope:       scope,
		Title:       fmt.Sprintf("%s reports %.1f%% endurance used", label, used),
		Remediation: "Confirm the standardized NVMe endurance counter and plan replacement before it reaches its limit.",
	}
}

func temperatureFinding(drive domain.Drive, label string, temperature float64, severity domain.Severity, scope string) domain.Finding {
	return domain.Finding{
		ID: "drive:" + drive.ID + ":temperature", Severity: severity, Scope: scope,
		Title:       fmt.Sprintf("%s temperature is %.1f C", label, temperature),
		Remediation: "Check airflow, fan operation, neighboring drive temperatures, and enclosure specifications.",
	}
}

func driveLabel(drive domain.Drive) string {
	if drive.Bay.Label != "" {
		return "Drive in " + drive.Bay.Label
	}
	if drive.Serial != "" {
		return "Drive " + drive.Serial
	}
	if drive.DevNode != "" {
		return "Drive " + drive.DevNode
	}
	return "Drive " + drive.ID
}

func pointerValue(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

func severityRank(value domain.Severity) int {
	switch value {
	case domain.SeverityCritical:
		return 0
	case domain.SeverityWarning:
		return 1
	case domain.SeverityUnknown:
		return 2
	case domain.SeverityInfo:
		return 3
	default:
		return 4
	}
}
