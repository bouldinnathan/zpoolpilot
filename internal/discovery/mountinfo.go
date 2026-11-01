// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const rootMountSource = "host.root-mount"

type mountInfoEntry struct {
	Root       string
	MountPoint string
	FSType     string
	Source     string
}

// parseRootMountInfo returns the one mount visible at /. mountinfo escapes
// whitespace and backslashes as octal sequences, so fields must be decoded
// before comparing the mountpoint or correlating a ZFS dataset source.
func parseRootMountInfo(data []byte) (mountInfoEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var root mountInfoEntry
	found := false
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		entry, err := parseMountInfoLine(line)
		if err != nil {
			return mountInfoEntry{}, fmt.Errorf("mountinfo line %d: %w", lineNumber, err)
		}
		if entry.MountPoint != "/" {
			continue
		}
		if found {
			return mountInfoEntry{}, fmt.Errorf("mountinfo contains multiple mounts at /")
		}
		root = entry
		found = true
	}
	if err := scanner.Err(); err != nil {
		return mountInfoEntry{}, fmt.Errorf("scan mountinfo: %w", err)
	}
	if !found {
		return mountInfoEntry{}, fmt.Errorf("mountinfo contains no mount at /")
	}
	return root, nil
}

func parseMountInfoLine(line string) (mountInfoEntry, error) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return mountInfoEntry{}, fmt.Errorf("record has %d fields, want at least 10", len(fields))
	}
	separator := -1
	for index := 6; index < len(fields); index++ {
		if fields[index] == "-" {
			separator = index
			break
		}
	}
	if separator < 0 || len(fields) != separator+4 {
		return mountInfoEntry{}, fmt.Errorf("record has an invalid optional-field separator")
	}
	if _, err := strconv.ParseUint(fields[0], 10, 64); err != nil {
		return mountInfoEntry{}, fmt.Errorf("invalid mount id %q", fields[0])
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return mountInfoEntry{}, fmt.Errorf("invalid parent mount id %q", fields[1])
	}
	if !validMountInfoDevice(fields[2]) {
		return mountInfoEntry{}, fmt.Errorf("invalid mount device %q", fields[2])
	}
	root, err := decodeMountInfoField(fields[3])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("invalid mount root: %w", err)
	}
	mountPoint, err := decodeMountInfoField(fields[4])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("invalid mountpoint: %w", err)
	}
	source, err := decodeMountInfoField(fields[separator+2])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("invalid mount source: %w", err)
	}
	fstype := fields[separator+1]
	if root == "" || mountPoint == "" || fstype == "" || source == "" || fields[5] == "" || fields[separator+3] == "" {
		return mountInfoEntry{}, fmt.Errorf("record contains an empty required field")
	}
	return mountInfoEntry{Root: root, MountPoint: mountPoint, FSType: fstype, Source: source}, nil
}

func validMountInfoDevice(value string) bool {
	major, minor, ok := strings.Cut(value, ":")
	if !ok || major == "" || minor == "" || strings.Contains(minor, ":") {
		return false
	}
	if _, err := strconv.ParseUint(major, 10, 64); err != nil {
		return false
	}
	_, err := strconv.ParseUint(minor, 10, 64)
	return err == nil
}

func decodeMountInfoField(value string) (string, error) {
	var result strings.Builder
	result.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			result.WriteByte(value[index])
			index++
			continue
		}
		if index+4 > len(value) {
			return "", fmt.Errorf("truncated escape in %q", value)
		}
		escape := value[index : index+4]
		switch escape {
		case `\040`:
			result.WriteByte(' ')
		case `\011`:
			result.WriteByte('\t')
		case `\012`:
			result.WriteByte('\n')
		case `\134`:
			result.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported escape %q", escape)
		}
		index += 4
	}
	return result.String(), nil
}

func (state *collectionState) collectRootMount() {
	data, err := state.collector.fs.ReadFile(state.options.MountInfoPath)
	if err != nil {
		state.addError(rootMountSource, classifyFilesystemError(err), err.Error(), true)
		return
	}
	root, err := parseRootMountInfo(data)
	if err != nil {
		state.addError(rootMountSource, "parse", err.Error(), true)
		return
	}
	if !strings.EqualFold(root.FSType, "zfs") {
		return
	}
	poolName := datasetPoolName(root.Source)
	if strings.TrimSpace(root.Source) != root.Source || poolName == "" || poolName == "-" || strings.EqualFold(poolName, "none") {
		state.addError(rootMountSource, "inconsistent", fmt.Sprintf("ZFS root mount source %q has no pool identity", root.Source), true)
		return
	}
	matches := 0
	matchIndex := -1
	for index := range state.snapshot.Pools {
		if state.snapshot.Pools[index].Name == poolName {
			matches++
			matchIndex = index
		}
	}
	if matches != 1 {
		state.addError(rootMountSource, "inconsistent", fmt.Sprintf("ZFS root mount source %q identifies pool %q, which matched %d discovered pools", root.Source, poolName, matches), true)
		return
	}
	state.snapshot.Pools[matchIndex].BootCritical = true
}
