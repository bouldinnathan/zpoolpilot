// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"fmt"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// RefreshTelemetryFindings recomputes drive-health age from the current clock.
// Cached findings cannot establish freshness after collection has stopped.
func RefreshTelemetryFindings(snapshot *domain.Snapshot, now time.Time, staleAfter time.Duration) {
	if snapshot == nil {
		return
	}
	retained := snapshot.Findings[:0]
	for _, finding := range snapshot.Findings {
		if strings.HasPrefix(finding.ID, "drive:") &&
			(strings.HasSuffix(finding.ID, ":smart-never-collected") || strings.HasSuffix(finding.ID, ":smart-stale") || strings.HasSuffix(finding.ID, ":smart-future")) {
			continue
		}
		retained = append(retained, finding)
	}
	snapshot.Findings = retained
	existing := make(map[string]bool, len(snapshot.Drives))
	for _, drive := range snapshot.Drives {
		finding := domain.Finding{Severity: domain.SeverityUnknown, Scope: "drive/" + drive.ID}
		age := now.Sub(drive.Health.FreshAt)
		switch {
		case drive.Health.FreshAt.IsZero():
			finding.ID = "drive:" + drive.ID + ":smart-never-collected"
			finding.Title = "Drive health has not been collected"
			finding.Evidence = drive.Health.Message
			if strings.TrimSpace(finding.Evidence) == "" {
				finding.Evidence = "unknown"
			}
			finding.Remediation = "Run the no-wake hardware collector and verify smartctl/controller access."
		case age < -maxFutureSnapshotSkew:
			finding.ID = "drive:" + drive.ID + ":smart-future"
			finding.Title = "Drive health timestamp is in the future"
			finding.Evidence = fmt.Sprintf("last parsed time is %s ahead of the collector clock", (-age).Round(time.Second))
			finding.Remediation = "Discard the cached telemetry, check the host clock, and run the hardware collector again."
		case staleAfter > 0 && age > staleAfter:
			finding.ID = "drive:" + drive.ID + ":smart-stale"
			finding.Title = "Drive health data is stale"
			finding.Evidence = fmt.Sprintf("last parsed %s ago", age.Round(time.Second))
			finding.Remediation = "Check the hardware collector timer and SMART/controller permissions."
		default:
			continue
		}
		if !existing[finding.ID] {
			snapshot.Findings = append(snapshot.Findings, finding)
			existing[finding.ID] = true
		}
	}
}
