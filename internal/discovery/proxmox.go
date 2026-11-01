// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"errors"
	"fmt"
	"strings"
)

// ParseProxmoxBootMode accepts only a single consistent mode reported for
// configured ESPs. Mixed and missing state are deliberately unknown.
func ParseProxmoxBootMode(output string) (string, error) {
	hasUEFI, hasGRUB := false, false
	for lineNumber, line := range strings.Split(strings.ToLower(output), "\n") {
		if !strings.Contains(line, "is configured with:") {
			continue
		}
		configured := strings.SplitN(line, "is configured with:", 2)[1]
		lineUEFI, lineGRUB := false, false
		for _, token := range strings.FieldsFunc(configured, func(character rune) bool {
			return !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-')
		}) {
			switch token {
			case "uefi", "systemd-boot":
				lineUEFI = true
			case "grub":
				lineGRUB = true
			}
		}
		if lineUEFI == lineGRUB {
			return "", fmt.Errorf("proxmox-boot-tool status line %d has an unrecognized or ambiguous configured mode", lineNumber+1)
		}
		hasUEFI = hasUEFI || lineUEFI
		hasGRUB = hasGRUB || lineGRUB
	}
	if hasUEFI == hasGRUB {
		return "", errors.New("proxmox-boot-tool status is missing a single consistent configured mode")
	}
	if hasGRUB {
		return "grub", nil
	}
	return "systemd-boot", nil
}
