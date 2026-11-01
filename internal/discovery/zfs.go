// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

var (
	errEmptyPoolsJSON    = errors.New("empty pools object requires independent no-pools evidence")
	errEmptyDatasetsJSON = errors.New("empty datasets object requires independent no-datasets evidence")
)

func parseZpoolVersion(data []byte) (string, error) {
	if root, err := decodeObject(data); err == nil {
		if versionObject := object(root["zfs_version"]); versionObject != nil {
			for _, key := range []string{"userland", "version", "kernel"} {
				if value := normalizeZFSVersion(text(versionObject[key])); value != "" {
					return value, nil
				}
			}
		}
		for _, key := range []string{"zfs_version", "zpool_version", "version", "userland_version"} {
			if value := normalizeZFSVersion(text(root[key])); value != "" {
				return value, nil
			}
		}
		for _, key := range sortedKeys(root) {
			if strings.Contains(strings.ToLower(key), "version") {
				if value := normalizeZFSVersion(text(root[key])); value != "" {
					return value, nil
				}
			}
		}
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "zfs-") && !strings.HasPrefix(lower, "zfs-kmod-") {
			return normalizeZFSVersion(line), nil
		}
		if strings.Contains(lower, "zfs") && strings.Contains(lower, "version") {
			if index := strings.IndexAny(line, ":="); index >= 0 {
				if value := strings.TrimSpace(line[index+1:]); value != "" {
					return normalizeZFSVersion(value), nil
				}
			}
			fields := strings.Fields(line)
			if len(fields) > 1 {
				return normalizeZFSVersion(fields[len(fields)-1]), nil
			}
		}
	}
	return "", errors.New("version was not present in output")
}

func normalizeZFSVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "zfs-kmod-")
	value = strings.TrimPrefix(value, "zfs-")
	return value
}

func parseZpoolStatusJSON(data []byte) ([]domain.Pool, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	rawPools, present := root["pools"]
	if !present {
		return nil, errors.New("missing pools object")
	}
	poolsObject, ok := rawPools.(map[string]any)
	if !ok {
		return nil, errors.New("pools is not an object")
	}
	if len(poolsObject) == 0 {
		return nil, errEmptyPoolsJSON
	}
	pools := make([]domain.Pool, 0, len(poolsObject))
	for _, key := range sortedKeys(poolsObject) {
		rawPool, ok := poolsObject[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pool %q is not an object", key)
		}
		pool := domain.Pool{
			Name:   text(first(rawPool, "name", "pool")),
			GUID:   text(rawPool["guid"]),
			State:  normalizedState(text(rawPool["state"])),
			Status: text(first(rawPool, "status", "msgid")),
			Action: text(rawPool["action"]),
		}
		if pool.Name == "" {
			pool.Name = strings.TrimSpace(key)
		}
		if pool.Name == "" {
			return nil, fmt.Errorf("pool %q is missing its name", key)
		}
		if pool.GUID == "" {
			return nil, fmt.Errorf("pool %q is missing its GUID", pool.Name)
		}
		if pool.State == "" {
			return nil, fmt.Errorf("pool %q is missing its state", pool.Name)
		}

		classes := []struct {
			jsonName string
			class    string
		}{
			{"vdevs", "data"},
			{"logs", "log"},
			{"log", "log"},
			{"cache", "cache"},
			{"spares", "spare"},
			{"spare", "spare"},
			{"special", "special"},
			{"dedup", "dedup"},
		}
		seenClass := make(map[string]bool)
		dataVDevsPresent := false
		for _, candidate := range classes {
			if seenClass[candidate.jsonName] {
				continue
			}
			seenClass[candidate.jsonName] = true
			rawVDevs, present := rawPool[candidate.jsonName]
			if !present {
				continue
			}
			vdevMap, ok := rawVDevs.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("pool %q %s vdevs is not an object", pool.Name, candidate.class)
			}
			if candidate.jsonName == "vdevs" {
				dataVDevsPresent = true
				if len(vdevMap) == 0 {
					return nil, fmt.Errorf("pool %q has an empty data vdev object", pool.Name)
				}
			}
			parsed, parseErr := parseVDevMap(vdevMap, candidate.class, pool.Name, candidate.jsonName == "vdevs")
			if parseErr != nil {
				return nil, fmt.Errorf("pool %q: %w", pool.Name, parseErr)
			}
			// The data class contains a synthetic pool-root object.  Keep
			// its counters on Pool and expose only its real children.
			if candidate.class == "data" && len(parsed) == 1 &&
				(parsed[0].Type == "root" || parsed[0].Name == pool.Name) {
				rootVDev := parsed[0]
				pool.ReadErrors = rootVDev.ReadErrors
				pool.WriteErrors = rootVDev.WriteErrors
				pool.ChecksumErrors = rootVDev.ChecksumErrors
				if rootVDev.AllocatedBytes > 0 {
					pool.AllocatedBytes = rootVDev.AllocatedBytes
				}
				if rootVDev.SizeBytes > 0 {
					pool.SizeBytes = rootVDev.SizeBytes
				}
				pool.VDevs = append(pool.VDevs, rootVDev.Children...)
			} else {
				pool.VDevs = append(pool.VDevs, parsed...)
			}
		}
		if !dataVDevsPresent {
			return nil, fmt.Errorf("pool %q is missing its data vdev object", pool.Name)
		}
		if len(pool.VDevs) == 0 {
			return nil, fmt.Errorf("pool %q contains no member vdevs", pool.Name)
		}
		if count, ok := unsigned(rawPool["error_count"]); ok && count > pool.ChecksumErrors {
			pool.ChecksumErrors = count
		}
		rawScan, scanPresent := rawPool["scan_stats"]
		if !scanPresent {
			rawScan, scanPresent = rawPool["scan"]
		}
		if !scanPresent {
			return nil, fmt.Errorf("pool %q is missing scan activity authority", pool.Name)
		}
		scanObject, ok := rawScan.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pool %q scan activity is not an object", pool.Name)
		}
		pool.Scan, err = parseScan(scanObject)
		if err != nil {
			return nil, fmt.Errorf("pool %q scan activity: %w", pool.Name, err)
		}
		pool.Trim = aggregatePoolTrim(pool.VDevs)
		pools = append(pools, pool)
	}
	return pools, nil
}

func parseVDevMap(values map[string]any, class, poolName string, allowPoolRoot bool) ([]domain.VDev, error) {
	result := make([]domain.VDev, 0, len(values))
	for _, key := range sortedKeys(values) {
		value, ok := values[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("vdev %q is not an object", key)
		}
		vdev, err := parseVDev(key, value, class, poolName, allowPoolRoot)
		if err != nil {
			return nil, err
		}
		result = append(result, vdev)
	}
	return result, nil
}

func parseVDev(key string, value map[string]any, inheritedClass, poolName string, allowPoolRoot bool) (domain.VDev, error) {
	var childrenMap map[string]any
	if rawChildren, present := value["vdevs"]; present {
		var ok bool
		childrenMap, ok = rawChildren.(map[string]any)
		if !ok {
			return domain.VDev{}, fmt.Errorf("vdev %q children is not an object", key)
		}
	}
	name := text(value["name"])
	if name == "" {
		name = key
	}
	typeName := strings.ToLower(text(first(value, "vdev_type", "type")))
	if typeName == "" {
		typeName = inferVDevType(name, len(childrenMap) > 0)
	}
	class := normalizeVDevClass(text(value["class"]))
	if class == "" {
		class = normalizeVDevClass(inheritedClass)
	}
	vdev := domain.VDev{
		Name:  name,
		GUID:  text(value["guid"]),
		Type:  typeName,
		Class: class,
		State: normalizedState(text(value["state"])),
		Path:  text(value["path"]),
	}
	isPoolRoot := allowPoolRoot && len(childrenMap) > 0 && (vdev.Type == "root" || vdev.Name == poolName)
	if vdev.Type == "root" && !isPoolRoot {
		return domain.VDev{}, fmt.Errorf("vdev %q is an unexpected root node", vdev.Name)
	}
	if !isPoolRoot {
		if vdev.GUID == "" {
			return domain.VDev{}, fmt.Errorf("vdev %q is missing its GUID", vdev.Name)
		}
		if vdev.State == "" {
			return domain.VDev{}, fmt.Errorf("vdev %q is missing its state", vdev.Name)
		}
	}
	vdev.ReadErrors, _ = unsigned(first(value, "read_errors", "read"))
	vdev.WriteErrors, _ = unsigned(first(value, "write_errors", "write"))
	vdev.ChecksumErrors, _ = unsigned(first(value, "checksum_errors", "cksum"))
	vdev.AllocatedBytes, _ = unsigned(first(value, "alloc_space", "allocated", "alloc"))
	vdev.SizeBytes, _ = unsigned(first(value, "total_space", "rep_dev_size", "phys_space", "size"))
	vdev.Trim = parseTrimStats(value)
	children, err := parseVDevMap(childrenMap, class, poolName, false)
	if err != nil {
		return domain.VDev{}, fmt.Errorf("vdev %q: %w", vdev.Name, err)
	}
	vdev.Children = children
	if len(vdev.Children) == 0 && vdev.Type == "disk" && vdev.Path == "" {
		return domain.VDev{}, fmt.Errorf("leaf disk vdev %q is missing its path", vdev.Name)
	}
	return vdev, nil
}

func parseTrimStats(value map[string]any) domain.TrimStats {
	rawUnsupported, present := value["trim_notsup"]
	if !present {
		return domain.TrimStats{}
	}
	notSupported, ok := unsigned(rawUnsupported)
	if !ok {
		return domain.TrimStats{State: "unknown"}
	}
	supported := notSupported == 0
	result := domain.TrimStats{Supported: &supported}
	if !supported {
		result.State = "unsupported"
		return result
	}
	result.State = normalizeTrimState(text(value["trim_state"]))
	result.BytesDone, _ = unsigned(value["trimmed"])
	result.BytesTotal, _ = unsigned(value["to_trim"])
	result.Errors, _ = unsigned(value["trim_errors"])
	result.ActionTime = parseFlexibleTime(value["trim_time"])
	result.Percent = trimPercent(result.State, result.BytesDone, result.BytesTotal)
	return result
}

func normalizeTrimState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "untrimmed":
		return "untrimmed"
	case "active", "trimming":
		return "active"
	case "suspended":
		return "suspended"
	case "complete", "completed":
		return "complete"
	case "canceled", "cancelled":
		return "canceled"
	case "unsupported":
		return "unsupported"
	case "unknown", "":
		return "unknown"
	default:
		return "unknown"
	}
}

func trimPercent(state string, done, total uint64) float64 {
	if state == "complete" {
		return 100
	}
	if total == 0 {
		return 0
	}
	percent := float64(done) * 100 / float64(total)
	if percent > 100 {
		return 100
	}
	return percent
}

func aggregatePoolTrim(vdevs []domain.VDev) domain.TrimStats {
	var leaves []domain.TrimStats
	var walk func([]domain.VDev)
	walk = func(values []domain.VDev) {
		for _, vdev := range values {
			if len(vdev.Children) > 0 {
				walk(vdev.Children)
				continue
			}
			leaves = append(leaves, vdev.Trim)
		}
	}
	walk(vdevs)
	if len(leaves) == 0 {
		return domain.TrimStats{State: "unknown"}
	}

	result := domain.TrimStats{}
	states := make(map[string]bool)
	supportedCount, unsupportedCount := 0, 0
	unknown := false
	minimumPercent := 100.0
	havePercent := false
	for _, leaf := range leaves {
		if leaf.Supported == nil || leaf.State == "" || leaf.State == "unknown" {
			unknown = true
			continue
		}
		if !*leaf.Supported {
			unsupportedCount++
			states["unsupported"] = true
			continue
		}
		supportedCount++
		states[leaf.State] = true
		result.BytesDone = saturatingAdd(result.BytesDone, leaf.BytesDone)
		result.BytesTotal = saturatingAdd(result.BytesTotal, leaf.BytesTotal)
		result.Errors = saturatingAdd(result.Errors, leaf.Errors)
		if leaf.ActionTime.After(result.ActionTime) {
			result.ActionTime = leaf.ActionTime
		}
		if leaf.Percent < minimumPercent {
			minimumPercent = leaf.Percent
		}
		havePercent = true
	}

	if unknown {
		result.State = "unknown"
		return result
	}
	if supportedCount > 0 && unsupportedCount == 0 {
		supported := true
		result.Supported = &supported
	} else if supportedCount == 0 && unsupportedCount > 0 {
		supported := false
		result.Supported = &supported
	}
	if len(states) != 1 {
		result.State = "mixed"
	} else {
		for state := range states {
			result.State = state
		}
	}
	if result.BytesTotal > 0 {
		result.Percent = trimPercent(result.State, result.BytesDone, result.BytesTotal)
	} else if havePercent {
		result.Percent = minimumPercent
	}
	if result.State == "complete" {
		result.Percent = 100
	}
	return result
}

func saturatingAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func inferVDevType(name string, hasChildren bool) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "mirror"):
		return "mirror"
	case strings.HasPrefix(lower, "raidz"):
		return "raidz"
	case strings.HasPrefix(lower, "draid"):
		return "draid"
	case hasChildren:
		return "root"
	default:
		return "disk"
	}
}

func parseScan(scan map[string]any) (domain.Scan, error) {
	function, err := requiredScanText(scan, "function", "scan_function")
	if err != nil {
		return domain.Scan{}, err
	}
	state, err := requiredScanText(scan, "state", "scan_state")
	if err != nil {
		return domain.Scan{}, err
	}
	function = normalizeScanToken(function)
	state = normalizeScanToken(state)
	switch function {
	case "none", "scrub", "resilver":
	default:
		return domain.Scan{}, fmt.Errorf("unrecognized function %q", function)
	}
	switch state {
	case "scanning", "running", "active", "in progress":
		state = "scanning"
	case "paused":
		// A paused scrub is still lifecycle activity and remains distinct in
		// monitoring output.
	case "finished", "complete", "completed":
		state = "finished"
	case "canceled", "cancelled":
		state = "canceled"
	case "none", "not requested":
		state = "none"
	default:
		return domain.Scan{}, fmt.Errorf("unrecognized state %q", state)
	}
	if function == "none" {
		if state != "none" {
			return domain.Scan{}, fmt.Errorf("function none is inconsistent with state %q", state)
		}
	} else if state == "none" {
		return domain.Scan{}, fmt.Errorf("function %q is inconsistent with state none", function)
	}
	result := domain.Scan{
		Function: function,
		State:    state,
		EndedAt:  parseFlexibleTime(first(scan, "end_time", "ended_at", "end")),
	}
	result.BytesExamined, _ = unsigned(first(scan, "examined", "bytes_examined", "issued"))
	result.BytesTotal, _ = unsigned(first(scan, "to_examine", "bytes_total", "total"))
	result.Errors, _ = unsigned(first(scan, "errors", "error_count"))
	if percent, ok := decimal(first(scan, "percent", "percentage", "pct_done")); ok {
		result.Percent = percent
	} else if result.BytesTotal > 0 {
		result.Percent = float64(result.BytesExamined) * 100 / float64(result.BytesTotal)
	}
	return result, nil
}

func requiredScanText(scan map[string]any, keys ...string) (string, error) {
	for _, key := range keys {
		value, present := scan[key]
		if !present {
			continue
		}
		textValue, ok := value.(string)
		if !ok || strings.TrimSpace(textValue) == "" {
			return "", fmt.Errorf("%s must be non-empty text", key)
		}
		return textValue, nil
	}
	return "", fmt.Errorf("missing %s", keys[0])
}

func normalizeScanToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", " ", "-", " ").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

type textVDev struct {
	vdev     domain.VDev
	indent   int
	children []*textVDev
}

// parseZpoolStatusText is the compatibility path for OpenZFS releases which
// do not support status JSON.  Locale must be C (a Runner responsibility).
func parseZpoolStatusText(data []byte) ([]domain.Pool, error) {
	var pools []domain.Pool
	var pool *domain.Pool
	var roots []*textVDev
	var stack []*textVDev
	inConfig := false
	class := "data"

	flush := func() {
		if pool == nil {
			return
		}
		pool.VDevs = nil
		for _, root := range roots {
			if root.vdev.Name == pool.Name {
				pool.ReadErrors = root.vdev.ReadErrors
				pool.WriteErrors = root.vdev.WriteErrors
				pool.ChecksumErrors = root.vdev.ChecksumErrors
				for _, child := range root.children {
					pool.VDevs = append(pool.VDevs, materializeTextVDev(child))
				}
			} else {
				pool.VDevs = append(pool.VDevs, materializeTextVDev(root))
			}
		}
		pool.Trim = aggregatePoolTrim(pool.VDevs)
		pools = append(pools, *pool)
		pool = nil
		roots = nil
		stack = nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "pool:") {
			flush()
			pool = &domain.Pool{Name: strings.TrimSpace(strings.TrimPrefix(trimmed, "pool:"))}
			inConfig = false
			continue
		}
		if pool == nil {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "state:"):
			pool.State = normalizedState(strings.TrimSpace(strings.TrimPrefix(trimmed, "state:")))
		case strings.HasPrefix(trimmed, "status:"):
			pool.Status = strings.TrimSpace(strings.TrimPrefix(trimmed, "status:"))
		case strings.HasPrefix(trimmed, "action:"):
			pool.Action = strings.TrimSpace(strings.TrimPrefix(trimmed, "action:"))
		case strings.HasPrefix(trimmed, "scan:"):
			parsedScan, parseErr := parseTextScan(strings.TrimSpace(strings.TrimPrefix(trimmed, "scan:")))
			if parseErr != nil {
				return nil, fmt.Errorf("pool %q scan activity: %w", pool.Name, parseErr)
			}
			pool.Scan = parsedScan
		case trimmed == "config:":
			inConfig = true
			class = "data"
			stack = nil
			continue
		case strings.HasPrefix(trimmed, "errors:"):
			inConfig = false
			continue
		}
		if !inConfig || trimmed == "" || strings.HasPrefix(trimmed, "NAME") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		if isVDevClassHeading(trimmed) {
			class = normalizeVDevClass(trimmed)
			stack = nil
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 || !looksLikeVDevState(fields[1]) {
			continue
		}
		indent := leadingColumns(line)
		node := &textVDev{
			indent: indent,
			vdev: domain.VDev{
				Name:  fields[0],
				Type:  inferVDevType(fields[0], false),
				Class: class,
				State: normalizedState(fields[1]),
				Trim:  parseTextTrim(trimmed),
			},
		}
		if strings.HasPrefix(fields[0], "/") {
			node.vdev.Path = fields[0]
		}
		if len(fields) >= 5 {
			node.vdev.ReadErrors, _ = unsigned(fields[2])
			node.vdev.WriteErrors, _ = unsigned(fields[3])
			node.vdev.ChecksumErrors, _ = unsigned(fields[4])
		}
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			roots = append(roots, node)
		} else {
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, node)
		}
		stack = append(stack, node)
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(pools) == 0 && !strings.Contains(strings.ToLower(string(data)), "no pools available") {
		return nil, errors.New("no pool records found")
	}
	for _, pool := range pools {
		if pool.Scan.State == "" {
			return nil, fmt.Errorf("pool %q is missing scan activity authority", pool.Name)
		}
	}
	return pools, nil
}

func parseTextTrim(line string) domain.TrimStats {
	open := strings.LastIndex(line, "(")
	close := strings.LastIndex(line, ")")
	if open < 0 || close <= open {
		return domain.TrimStats{}
	}
	status := strings.TrimSpace(line[open+1 : close])
	lower := strings.ToLower(status)
	switch lower {
	case "trim unsupported":
		supported := false
		return domain.TrimStats{State: "unsupported", Supported: &supported}
	case "untrimmed":
		supported := true
		return domain.TrimStats{State: "untrimmed", Supported: &supported}
	case "trimming":
		supported := true
		return domain.TrimStats{State: "active", Supported: &supported}
	}
	marker := strings.Index(lower, "% trimmed")
	if marker <= 0 {
		return domain.TrimStats{}
	}
	percent, ok := decimal(strings.TrimSpace(status[:marker]))
	if !ok || percent < 0 || percent > 100 {
		return domain.TrimStats{State: "unknown"}
	}
	supported := true
	result := domain.TrimStats{Supported: &supported, Percent: percent}
	remainder := strings.TrimSpace(status[marker+len("% trimmed"):])
	remainder = strings.TrimSpace(strings.TrimPrefix(remainder, ","))
	lowerRemainder := strings.ToLower(remainder)
	var timestamp string
	switch {
	case strings.HasPrefix(lowerRemainder, "suspended, started at "):
		result.State = "suspended"
		timestamp = remainder[len("suspended, started at "):]
	case strings.HasPrefix(lowerRemainder, "started at "):
		result.State = "active"
		timestamp = remainder[len("started at "):]
	case strings.HasPrefix(lowerRemainder, "completed at "):
		result.State = "complete"
		timestamp = remainder[len("completed at "):]
	default:
		result.State = "unknown"
	}
	result.ActionTime = parseFlexibleTime(strings.TrimSpace(timestamp))
	return result
}

func materializeTextVDev(node *textVDev) domain.VDev {
	result := node.vdev
	for _, child := range node.children {
		result.Children = append(result.Children, materializeTextVDev(child))
	}
	if len(result.Children) > 0 && result.Type == "disk" {
		result.Type = "root"
	}
	return result
}

func leadingColumns(line string) int {
	columns := 0
	for _, char := range line {
		switch char {
		case ' ':
			columns++
		case '\t':
			columns += 8
		default:
			return columns
		}
	}
	return columns
}

func isVDevClassHeading(value string) bool {
	switch strings.ToLower(strings.TrimSuffix(value, ":")) {
	case "logs", "log", "cache", "spares", "spare", "special", "dedup":
		return true
	default:
		return false
	}
}

func normalizeVDevClass(value string) string {
	value = strings.ToLower(strings.TrimSuffix(value, ":"))
	switch value {
	case "normal":
		return "data"
	case "logs":
		return "log"
	case "spares":
		return "spare"
	default:
		return value
	}
}

func looksLikeVDevState(value string) bool {
	switch normalizedState(value) {
	case "ONLINE", "DEGRADED", "FAULTED", "OFFLINE", "UNAVAIL", "REMOVED", "AVAIL", "INUSE":
		return true
	default:
		return false
	}
}

func parseTextScan(value string) (domain.Scan, error) {
	lower := strings.ToLower(value)
	result := domain.Scan{}
	switch {
	case strings.Contains(lower, "none requested"):
		result.Function = "none"
	case strings.Contains(lower, "scrub"):
		result.Function = "scrub"
	case strings.Contains(lower, "resilver"):
		result.Function = "resilver"
	case strings.Contains(lower, "trim"):
		result.Function = "trim"
	}
	switch {
	case strings.Contains(lower, "none requested"):
		result.State = "none"
	case strings.Contains(lower, "in progress"):
		result.State = "scanning"
	case strings.Contains(lower, "paused"):
		result.State = "paused"
	case strings.Contains(lower, "completed"), strings.Contains(lower, "finished"):
		result.State = "finished"
	case strings.Contains(lower, "canceled"):
		result.State = "canceled"
	case result.Function == "scrub" && strings.Contains(lower, "repaired"):
		result.State = "finished"
	case result.Function == "resilver" && strings.Contains(lower, "resilvered"):
		result.State = "finished"
	}
	for _, field := range strings.Fields(lower) {
		if strings.HasSuffix(field, "%") {
			if percent, ok := decimal(field); ok {
				result.Percent = percent
			}
		}
	}
	if result.Function == "" || result.State == "" {
		return domain.Scan{}, fmt.Errorf("unrecognized status %q", value)
	}
	return result, nil
}

func parseZpoolListJSON(data []byte) ([]domain.Pool, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	rawPools, present := root["pools"]
	if !present {
		return nil, errors.New("missing pools object")
	}
	poolsObject, ok := rawPools.(map[string]any)
	if !ok {
		return nil, errors.New("pools is not an object")
	}
	if len(poolsObject) == 0 {
		return nil, errEmptyPoolsJSON
	}
	result := make([]domain.Pool, 0, len(poolsObject))
	seenNames := make(map[string]bool, len(poolsObject))
	for _, key := range sortedKeys(poolsObject) {
		record, ok := poolsObject[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pool %q is not an object", key)
		}
		properties, err := requiredJSONProperties(record, "pool "+fmt.Sprintf("%q", key), "size", "allocated", "free", "fragmentation", "capacity", "health")
		if err != nil {
			return nil, err
		}
		pool := domain.Pool{Name: text(record["name"]), GUID: text(record["guid"]), State: normalizedState(text(record["state"]))}
		if pool.Name == "" {
			pool.Name = strings.TrimSpace(key)
		}
		if pool.Name == "" {
			return nil, fmt.Errorf("pool %q is missing its name", key)
		}
		if seenNames[pool.Name] {
			return nil, fmt.Errorf("duplicate pool name %q", pool.Name)
		}
		seenNames[pool.Name] = true
		if pool.GUID == "" {
			pool.GUID = textProperty(properties, "guid")
		} else if propertyGUID := textProperty(properties, "guid"); propertyGUID != "" && propertyGUID != pool.GUID {
			return nil, fmt.Errorf("pool %q has conflicting GUID fields (%s != %s)", pool.Name, pool.GUID, propertyGUID)
		}
		if pool.GUID == "" {
			return nil, fmt.Errorf("pool %q is missing its GUID", pool.Name)
		}
		var valid bool
		if pool.SizeBytes, valid = unsignedProperty(properties, "size"); !valid {
			return nil, fmt.Errorf("pool %q has an invalid size property", pool.Name)
		}
		if pool.AllocatedBytes, valid = unsignedProperty(properties, "allocated"); !valid {
			return nil, fmt.Errorf("pool %q has an invalid allocated property", pool.Name)
		}
		if pool.FreeBytes, valid = unsignedProperty(properties, "free"); !valid {
			return nil, fmt.Errorf("pool %q has an invalid free property", pool.Name)
		}
		if pool.CapacityPercent, valid = decimalProperty(properties, "capacity"); !valid {
			return nil, fmt.Errorf("pool %q has an invalid capacity property", pool.Name)
		}
		if pool.FragmentationPct, valid = decimalProperty(properties, "fragmentation"); !valid {
			return nil, fmt.Errorf("pool %q has an invalid fragmentation property", pool.Name)
		}
		if health := normalizedState(textProperty(properties, "health")); health != "" {
			if pool.State != "" && pool.State != health {
				return nil, fmt.Errorf("pool %q has conflicting state fields (%s != %s)", pool.Name, pool.State, health)
			}
			pool.State = health
		}
		if pool.State == "" {
			return nil, fmt.Errorf("pool %q is missing its state", pool.Name)
		}
		result = append(result, pool)
	}
	return result, nil
}

func parseZpoolListText(data []byte) ([]domain.Pool, error) {
	var result []domain.Pool
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.Contains(strings.ToLower(line), "no pools available") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 8 {
			fields = strings.Fields(line)
		}
		if len(fields) < 8 || strings.EqualFold(fields[0], "name") {
			continue
		}
		pool := domain.Pool{Name: fields[0], GUID: fields[1], State: normalizedState(fields[7])}
		pool.SizeBytes, _ = unsigned(fields[2])
		pool.AllocatedBytes, _ = unsigned(fields[3])
		pool.FreeBytes, _ = unsigned(fields[4])
		pool.FragmentationPct, _ = decimal(fields[5])
		pool.CapacityPercent, _ = decimal(fields[6])
		result = append(result, pool)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 && !strings.Contains(strings.ToLower(string(data)), "no pools available") {
		return nil, errors.New("no pool-list records found")
	}
	return result, nil
}

func unsignedProperty(properties map[string]any, name string) (uint64, bool) {
	value, ok := propertyValue(properties, name)
	if !ok {
		return 0, false
	}
	return unsigned(value)
}

func decimalProperty(properties map[string]any, name string) (float64, bool) {
	value, ok := propertyValue(properties, name)
	if !ok {
		return 0, false
	}
	return decimal(value)
}

func textProperty(properties map[string]any, name string) string {
	value, ok := propertyValue(properties, name)
	if !ok {
		return ""
	}
	return text(value)
}

func parseZpoolGetJSON(data []byte) (map[string]map[string]string, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	rawPools, present := root["pools"]
	if !present {
		return nil, errors.New("missing pools object")
	}
	poolsObject, ok := rawPools.(map[string]any)
	if !ok {
		return nil, errors.New("pools is not an object")
	}
	if len(poolsObject) == 0 {
		return nil, errEmptyPoolsJSON
	}
	result := make(map[string]map[string]string, len(poolsObject))
	for _, key := range sortedKeys(poolsObject) {
		record, ok := poolsObject[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pool %q is not an object", key)
		}
		name := text(record["name"])
		if name == "" {
			name = strings.TrimSpace(key)
		}
		if name == "" {
			return nil, fmt.Errorf("pool %q is missing its name", key)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("duplicate pool name %q", name)
		}
		properties, err := requiredJSONProperties(record, "pool "+fmt.Sprintf("%q", name), "guid", "size", "allocated", "free", "fragmentation", "capacity", "health", "bootfs")
		if err != nil {
			return nil, err
		}
		values := make(map[string]string, len(properties))
		for property := range properties {
			values[property] = textProperty(properties, property)
		}
		if guid := text(record["guid"]); guid != "" {
			if propertyGUID := values["guid"]; propertyGUID != "" && propertyGUID != guid {
				return nil, fmt.Errorf("pool %q has conflicting GUID fields (%s != %s)", name, guid, propertyGUID)
			}
			values["guid"] = guid
		}
		if values["guid"] == "" {
			return nil, fmt.Errorf("pool %q is missing its GUID", name)
		}
		if values["health"] == "" {
			return nil, fmt.Errorf("pool %q has an invalid health property", name)
		}
		if values["bootfs"] == "" {
			return nil, fmt.Errorf("pool %q has an invalid bootfs property", name)
		}
		for _, property := range []string{"size", "allocated", "free"} {
			if _, valid := unsigned(values[property]); !valid {
				return nil, fmt.Errorf("pool %q has an invalid %s property", name, property)
			}
		}
		for _, property := range []string{"fragmentation", "capacity"} {
			if _, valid := decimal(values[property]); !valid {
				return nil, fmt.Errorf("pool %q has an invalid %s property", name, property)
			}
		}
		result[name] = values
	}
	return result, nil
}

func parseZpoolGetText(data []byte) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 3 {
			continue
		}
		if result[fields[0]] == nil {
			result[fields[0]] = make(map[string]string)
		}
		result[fields[0]][fields[1]] = fields[2]
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 && !strings.Contains(strings.ToLower(string(data)), "no pools available") {
		return nil, errors.New("no pool-property records found")
	}
	return result, nil
}

func parseZFSGetJSON(data []byte) ([]domain.Dataset, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	rawDatasets, present := root["datasets"]
	if !present {
		return nil, errors.New("missing datasets object")
	}
	datasetObject, ok := rawDatasets.(map[string]any)
	if !ok {
		return nil, errors.New("datasets is not an object")
	}
	if len(datasetObject) == 0 {
		return nil, errEmptyDatasetsJSON
	}
	result := make([]domain.Dataset, 0, len(datasetObject))
	seenNames := make(map[string]bool, len(datasetObject))
	for _, key := range sortedKeys(datasetObject) {
		record, ok := datasetObject[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("dataset %q is not an object", key)
		}
		properties, err := requiredJSONProperties(record, "dataset "+fmt.Sprintf("%q", key), "type", "guid")
		if err != nil {
			return nil, err
		}
		dataset := domain.Dataset{
			Name:            text(record["name"]),
			Type:            strings.ToLower(text(record["type"])),
			GUID:            text(record["guid"]),
			Properties:      make(map[string]string, len(properties)),
			PropertySources: make(map[string]string, len(properties)),
		}
		if dataset.Name == "" {
			dataset.Name = strings.TrimSpace(key)
		}
		if dataset.Name == "" {
			return nil, fmt.Errorf("dataset %q is missing its name", key)
		}
		if seenNames[dataset.Name] {
			return nil, fmt.Errorf("duplicate dataset name %q", dataset.Name)
		}
		seenNames[dataset.Name] = true
		for property := range properties {
			value, _ := propertyValue(properties, property)
			dataset.Properties[property] = text(value)
			dataset.PropertySources[property] = propertySource(properties, property)
		}
		if dataset.Type == "" {
			dataset.Type = strings.ToLower(dataset.Properties["type"])
		} else if propertyType := strings.ToLower(dataset.Properties["type"]); propertyType != "" && propertyType != dataset.Type {
			return nil, fmt.Errorf("dataset %q has conflicting type fields (%s != %s)", dataset.Name, dataset.Type, propertyType)
		}
		if dataset.GUID == "" {
			dataset.GUID = dataset.Properties["guid"]
		} else if propertyGUID := dataset.Properties["guid"]; propertyGUID != "" && propertyGUID != dataset.GUID {
			return nil, fmt.Errorf("dataset %q has conflicting GUID fields (%s != %s)", dataset.Name, dataset.GUID, propertyGUID)
		}
		if dataset.Type == "" {
			return nil, fmt.Errorf("dataset %q is missing its type", dataset.Name)
		}
		if dataset.GUID == "" {
			return nil, fmt.Errorf("dataset %q is missing its GUID", dataset.Name)
		}
		if dataset.Type == "filesystem" {
			if _, present := properties["mountpoint"]; !present || dataset.Properties["mountpoint"] == "" {
				return nil, fmt.Errorf("filesystem dataset %q is missing its mountpoint property", dataset.Name)
			}
		}
		dataset.UsedBytes, _ = unsigned(dataset.Properties["used"])
		dataset.AvailableBytes, _ = unsigned(firstString(dataset.Properties, "available", "avail"))
		dataset.ReferencedBytes, _ = unsigned(firstString(dataset.Properties, "referenced", "refer"))
		dataset.LogicalUsedBytes, _ = unsigned(firstString(dataset.Properties, "logicalused", "logical_used"))
		dataset.CompressionRatio, _ = decimal(firstString(dataset.Properties, "compressratio", "compressionratio"))
		result = append(result, dataset)
	}
	return result, nil
}

func requiredJSONProperties(record map[string]any, owner string, required ...string) (map[string]any, error) {
	rawProperties, present := record["properties"]
	if !present {
		return nil, fmt.Errorf("%s is missing its properties object", owner)
	}
	properties, ok := rawProperties.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s properties is not an object", owner)
	}
	for _, name := range sortedKeys(properties) {
		property, ok := properties[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s property %q is not an object", owner, name)
		}
		if _, present := property["value"]; !present {
			return nil, fmt.Errorf("%s property %q is missing its value", owner, name)
		}
	}
	for _, name := range required {
		if _, present := properties[name]; !present {
			return nil, fmt.Errorf("%s is missing required property %q", owner, name)
		}
	}
	return properties, nil
}

func firstString(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return ""
}

func parseZFSGetText(data []byte) ([]domain.Dataset, error) {
	byName := make(map[string]*domain.Dataset)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Property output can be large on hosts with user properties.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 4 {
			continue
		}
		name, property, value, source := fields[0], fields[1], fields[2], fields[3]
		dataset := byName[name]
		if dataset == nil {
			dataset = &domain.Dataset{Name: name, Properties: make(map[string]string), PropertySources: make(map[string]string)}
			byName[name] = dataset
		}
		dataset.Properties[property] = value
		dataset.PropertySources[property] = strings.ToLower(source)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	result := make([]domain.Dataset, 0, len(byName))
	for _, dataset := range byName {
		dataset.Type = strings.ToLower(dataset.Properties["type"])
		dataset.GUID = dataset.Properties["guid"]
		dataset.UsedBytes, _ = unsigned(dataset.Properties["used"])
		dataset.AvailableBytes, _ = unsigned(firstString(dataset.Properties, "available", "avail"))
		dataset.ReferencedBytes, _ = unsigned(firstString(dataset.Properties, "referenced", "refer"))
		dataset.LogicalUsedBytes, _ = unsigned(firstString(dataset.Properties, "logicalused", "logical_used"))
		dataset.CompressionRatio, _ = decimal(firstString(dataset.Properties, "compressratio", "compressionratio"))
		result = append(result, *dataset)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) == 0 && !strings.Contains(strings.ToLower(string(data)), "no datasets available") {
		return nil, errors.New("no dataset-property records found")
	}
	return result, nil
}

// mergePools overlays non-empty observations without discarding detailed vdev
// data gathered by another zpool subcommand.
func mergePools(target map[string]*domain.Pool, incoming []domain.Pool) {
	for index := range incoming {
		value := incoming[index]
		if value.Name == "" {
			continue
		}
		current := target[value.Name]
		if current == nil {
			copyValue := value
			target[value.Name] = &copyValue
			continue
		}
		if value.GUID != "" {
			current.GUID = value.GUID
		}
		if value.State != "" {
			current.State = value.State
		}
		if value.Status != "" {
			current.Status = value.Status
		}
		if value.Action != "" {
			current.Action = value.Action
		}
		if value.SizeBytes > 0 {
			current.SizeBytes = value.SizeBytes
		}
		if value.AllocatedBytes > 0 {
			current.AllocatedBytes = value.AllocatedBytes
		}
		if value.FreeBytes > 0 || value.SizeBytes > 0 {
			current.FreeBytes = value.FreeBytes
		}
		if value.CapacityPercent > 0 || value.AllocatedBytes == 0 {
			current.CapacityPercent = value.CapacityPercent
		}
		if value.FragmentationPct > 0 {
			current.FragmentationPct = value.FragmentationPct
		}
		if value.ReadErrors > 0 {
			current.ReadErrors = value.ReadErrors
		}
		if value.WriteErrors > 0 {
			current.WriteErrors = value.WriteErrors
		}
		if value.ChecksumErrors > 0 {
			current.ChecksumErrors = value.ChecksumErrors
		}
		if value.Scan.Function != "" || value.Scan.State != "" {
			current.Scan = value.Scan
		}
		if value.Trim.State != "" || value.Trim.Supported != nil {
			current.Trim = value.Trim
		}
		if len(value.VDevs) > 0 {
			current.VDevs = value.VDevs
		}
	}
}

func applyPoolProperties(pools map[string]*domain.Pool, properties map[string]map[string]string) {
	for name, values := range properties {
		pool := pools[name]
		if pool == nil {
			pool = &domain.Pool{Name: name}
			pools[name] = pool
		}
		if pool.GUID == "" {
			pool.GUID = values["guid"]
		}
		if value, ok := unsigned(values["size"]); ok {
			pool.SizeBytes = value
		}
		if value, ok := unsigned(values["allocated"]); ok {
			pool.AllocatedBytes = value
		}
		if value, ok := unsigned(values["free"]); ok {
			pool.FreeBytes = value
		}
		if value, ok := decimal(values["capacity"]); ok {
			pool.CapacityPercent = value
		}
		if value, ok := decimal(values["fragmentation"]); ok {
			pool.FragmentationPct = value
		}
		if health := normalizedState(values["health"]); health != "" {
			pool.State = health
		}
		bootfs := strings.TrimSpace(values["bootfs"])
		if bootfs != "" && bootfs != "-" && !strings.EqualFold(bootfs, "none") {
			pool.BootCritical = true
		}
	}
}

func applyIOStatText(pools map[string]*domain.Pool, data []byte) error {
	type sample struct {
		allocated, free uint64
		rates           domain.IOStats
	}
	samples := make(map[string]sample, len(pools))
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.EqualFold(line, "no pools available") && len(pools) == 0 {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			fields = strings.Fields(line)
		}
		if len(fields) != 7 {
			return fmt.Errorf("iostat record has %d fields, want 7", len(fields))
		}
		name := fields[0]
		if pools[name] == nil {
			return fmt.Errorf("iostat returned undiscovered pool %q", name)
		}
		if _, duplicate := samples[name]; duplicate {
			return fmt.Errorf("iostat returned duplicate pool %q", name)
		}
		var current sample
		var valid bool
		if current.allocated, valid = unsigned(fields[1]); !valid {
			return fmt.Errorf("iostat pool %q has invalid allocated bytes", name)
		}
		if current.free, valid = unsigned(fields[2]); !valid {
			return fmt.Errorf("iostat pool %q has invalid free bytes", name)
		}
		rates := []*float64{&current.rates.ReadOperationsPerSecond, &current.rates.WriteOperationsPerSecond,
			&current.rates.ReadBytesPerSecond, &current.rates.WriteBytesPerSecond}
		// zpool iostat -H -p columns following alloc/free are read ops,
		// write ops, read bandwidth, and write bandwidth.
		for index, rate := range rates {
			if *rate, valid = decimal(fields[index+3]); !valid || *rate < 0 {
				return fmt.Errorf("iostat pool %q has invalid rate in column %d", name, index+4)
			}
		}
		samples[name] = current
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	var missing []string
	for name := range pools {
		if _, present := samples[name]; !present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("iostat omitted pools: %s", strings.Join(missing, ", "))
	}
	// Commit rates only after every imported pool has a complete sample.
	for name, sample := range samples {
		pool := pools[name]
		pool.IO = sample.rates
		if pool.AllocatedBytes == 0 {
			pool.AllocatedBytes = sample.allocated
		}
		if pool.FreeBytes == 0 {
			pool.FreeBytes = sample.free
		}
	}
	return nil
}

func finalizePools(values map[string]*domain.Pool) []domain.Pool {
	result := make([]domain.Pool, 0, len(values))
	for _, pool := range values {
		if pool.SizeBytes > 0 && pool.CapacityPercent == 0 && pool.AllocatedBytes > 0 {
			pool.CapacityPercent = float64(pool.AllocatedBytes) * 100 / float64(pool.SizeBytes)
		}
		if pool.FreeBytes == 0 && pool.SizeBytes >= pool.AllocatedBytes {
			pool.FreeBytes = pool.SizeBytes - pool.AllocatedBytes
		}
		sortVDevs(pool.VDevs)
		result = append(result, *pool)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func sortVDevs(values []domain.VDev) {
	for index := range values {
		sortVDevs(values[index].Children)
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].Class != values[j].Class {
			return values[i].Class < values[j].Class
		}
		return values[i].Name < values[j].Name
	})
}

func markBootPoolsFromDatasets(pools []domain.Pool, datasets []domain.Dataset) {
	bootPools := make(map[string]bool)
	for _, dataset := range datasets {
		mountpoint := dataset.Properties["mountpoint"]
		if mountpoint == "/" {
			name := dataset.Name
			if slash := strings.IndexByte(name, '/'); slash >= 0 {
				name = name[:slash]
			}
			bootPools[name] = true
		}
	}
	for index := range pools {
		pools[index].BootCritical = pools[index].BootCritical || bootPools[pools[index].Name]
	}
}
