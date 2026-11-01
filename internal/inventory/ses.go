// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidSESDevicePath accepts only direct Linux SCSI-generic or bsg device
// names. Symlinks and live device type are checked separately at the point of
// use.
func ValidSESDevicePath(path string) bool {
	if path == "" || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n\t") {
		return false
	}
	if strings.HasPrefix(path, "/dev/sg") {
		number := strings.TrimPrefix(path, "/dev/sg")
		return number != "" && strings.Trim(number, "0123456789") == ""
	}
	if strings.HasPrefix(path, "/dev/bsg/") {
		address := strings.TrimPrefix(path, "/dev/bsg/")
		parts := strings.Split(address, ":")
		if len(parts) != 4 {
			return false
		}
		for _, part := range parts {
			if part == "" || strings.Trim(part, "0123456789") != "" {
				return false
			}
		}
		return true
	}
	return false
}

// CanonicalSESWWN validates a durable enclosure WWN and returns its persisted
// wire identity. Udev commonly prefixes WWNs with 0x; the stored form does
// not, and hexadecimal digits are always lowercase.
func CanonicalSESWWN(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "0x")
	if !reliableHexIdentifier(value) {
		return "", false
	}
	return "wwn:" + value, true
}

// CanonicalDriveWWN accepts the normalized forms Linux exposes for disk WWNs.
// In addition to plain hexadecimal NAA/EUI values, sysfs and NVMe by-id names
// may retain an explicit eui. or naa. designator.
func CanonicalDriveWWN(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "0x")
	if reliableHexIdentifier(value) {
		return value, true
	}
	for _, prefix := range []string{"eui.", "naa."} {
		if strings.HasPrefix(value, prefix) && reliableHexIdentifier(strings.TrimPrefix(value, prefix)) {
			return value, true
		}
	}
	return "", false
}

func reliableHexIdentifier(value string) bool {
	if len(value) < 16 || len(value) > 64 || len(value)%2 != 0 {
		return false
	}
	allZero, allF := true, true
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
		allZero = allZero && character == '0'
		allF = allF && character == 'f'
	}
	return !allZero && !allF
}

// CanonicalSESSerial validates an enclosure serial and returns the exact
// trimmed hardware value with its wire prefix. It rejects common firmware and
// factory placeholders which are non-unique despite looking nonempty.
func CanonicalSESSerial(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 6 || len(value) > 512 || !utf8.ValidString(value) {
		return "", false
	}
	compactRunes := make([]rune, 0, len(value))
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return "", false
		}
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			compactRunes = append(compactRunes, unicode.ToLower(character))
		}
	}
	if len(compactRunes) == 0 || allSameRunes(compactRunes) {
		return "", false
	}
	compact := string(compactRunes)
	if serialPlaceholder(compact) || sequentialDecimal(compact) {
		return "", false
	}
	return "scsi-serial:" + value, true
}

// ValidSESEnclosureIdentity accepts only the canonical wire representation
// produced by CanonicalSESWWN or CanonicalSESSerial.
func ValidSESEnclosureIdentity(identity string) bool {
	if strings.HasPrefix(identity, "wwn:") {
		canonical, ok := CanonicalSESWWN(strings.TrimPrefix(identity, "wwn:"))
		return ok && canonical == identity
	}
	if strings.HasPrefix(identity, "scsi-serial:") {
		canonical, ok := CanonicalSESSerial(strings.TrimPrefix(identity, "scsi-serial:"))
		return ok && canonical == identity
	}
	return false
}

func allSameRunes(value []rune) bool {
	for index := 1; index < len(value); index++ {
		if value[index] != value[0] {
			return false
		}
	}
	return true
}

func serialPlaceholder(compact string) bool {
	known := map[string]bool{
		"unknown": true, "default": true, "generic": true, "serial": true,
		"none": true, "null": true, "na": true, "notavailable": true,
		"tobefilledbyoem": true, "tobefilledbyoriginalequipmentmanufacturer": true,
	}
	if known[compact] {
		return true
	}
	withoutNumericSuffix := strings.TrimRight(compact, "0123456789")
	return withoutNumericSuffix != compact && known[withoutNumericSuffix]
}

func sequentialDecimal(value string) bool {
	if len(value) < 6 || strings.Trim(value, "0123456789") != "" {
		return false
	}
	ascending, descending := true, true
	for index := 1; index < len(value); index++ {
		previous, current := value[index-1]-'0', value[index]-'0'
		ascending = ascending && current == (previous+1)%10
		descending = descending && current == (previous+9)%10
	}
	return ascending || descending
}
