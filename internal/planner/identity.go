// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"strconv"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
)

// reliableDriveIdentity verifies that discovery's stable ID is derived from a
// non-placeholder WWN, or from an unambiguous reliable serial fallback.
func reliableDriveIdentity(drive domain.Drive) bool {
	if drive.WWN != "" {
		canonical, ok := inventory.CanonicalDriveWWN(drive.WWN)
		return ok && canonical == drive.WWN && drive.ID == "wwn:"+canonical
	}
	if drive.Serial == "" || drive.CapacityBytes == 0 || strings.Contains(drive.Serial, "|") || strings.Contains(drive.Model, "|") {
		return false
	}
	if _, ok := inventory.CanonicalSESSerial(drive.Serial); !ok {
		return false
	}
	return drive.ID == serialFallbackID(drive.Serial, drive.Model, drive.CapacityBytes)
}

func reliableDriveRefIdentity(ref domain.DriveRef) bool {
	if ref.WWN != "" {
		canonical, ok := inventory.CanonicalDriveWWN(ref.WWN)
		return ok && canonical == ref.WWN && ref.ID == "wwn:"+canonical
	}
	if ref.Serial == "" || ref.CapacityBytes == 0 || strings.Contains(ref.Serial, "|") {
		return false
	}
	if _, ok := inventory.CanonicalSESSerial(ref.Serial); !ok {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(ref.ID, "serial:"), "|")
	return strings.HasPrefix(ref.ID, "serial:") && len(parts) == 3 &&
		parts[0] == strings.ToLower(ref.Serial) && parts[2] == strconv.FormatUint(ref.CapacityBytes, 10)
}

func serialFallbackID(serial, model string, capacity uint64) string {
	return "serial:" + strings.ToLower(strings.Join([]string{serial, model, strconv.FormatUint(capacity, 10)}, "|"))
}
