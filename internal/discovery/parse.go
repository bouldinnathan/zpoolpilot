// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("JSON document is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON document contains multiple values")
		}
		return nil, fmt.Errorf("decode trailing JSON data: %w", err)
	}
	return value, nil
}

func object(value any) map[string]any {
	if value == nil {
		return nil
	}
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func array(value any) []any {
	if value == nil {
		return nil
	}
	if result, ok := value.([]any); ok {
		return result
	}
	return nil
}

func first(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value
		}
	}
	return nil
}

func text(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return string(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(value), 'f', -1, 32)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case uint64:
		return strconv.FormatUint(value, 10)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

func boolean(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case json.Number:
		n, err := strconv.ParseInt(string(value), 10, 64)
		return n != 0, err == nil
	case float64:
		return value != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "yes", "true", "on", "passed", "healthy":
			return true, true
		case "0", "no", "false", "off", "failed", "unhealthy":
			return false, true
		}
	}
	return false, false
}

func unsigned(value any) (uint64, bool) {
	switch value := value.(type) {
	case json.Number:
		if n, err := strconv.ParseUint(string(value), 10, 64); err == nil {
			return n, true
		}
	case float64:
		if validUintFloat(value) {
			return uint64(value), true
		}
	case float32:
		if converted := float64(value); validUintFloat(converted) {
			return uint64(converted), true
		}
	case int:
		if value >= 0 {
			return uint64(value), true
		}
	case int64:
		if value >= 0 {
			return uint64(value), true
		}
	case uint64:
		return value, true
	case string:
		return parseHumanNumber(value)
	}
	return 0, false
}

func validUintFloat(value float64) bool {
	// float64(^uint64(0)) rounds to 2^64, so use an exclusive 2^64 bound.
	return value >= 0 && value < math.Exp2(64) && !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value
}

// parseHumanNumber accepts exact integers and the binary unit spelling used by
// OpenZFS (for example 1.50T, 512K, and 12KiB).
func parseHumanNumber(input string) (uint64, bool) {
	input = strings.TrimSpace(input)
	if input == "" || input == "-" || strings.EqualFold(input, "none") {
		return 0, false
	}
	if value, err := strconv.ParseUint(input, 10, 64); err == nil {
		return value, true
	}

	upper := strings.ToUpper(input)
	upper = strings.TrimSuffix(upper, "IB")
	upper = strings.TrimSuffix(upper, "B")
	if upper == "" {
		return 0, false
	}
	multiplier := float64(1)
	last := upper[len(upper)-1]
	if index := strings.IndexByte("KMGTPEZ", last); index >= 0 {
		multiplier = math.Pow(1024, float64(index+1))
		upper = upper[:len(upper)-1]
	}
	value, err := strconv.ParseFloat(upper, 64)
	bytes := value * multiplier
	if err != nil || value < 0 || math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes >= math.Exp2(64) {
		return 0, false
	}
	return uint64(bytes), true
}

func decimal(value any) (float64, bool) {
	switch value := value.(type) {
	case json.Number:
		result, err := strconv.ParseFloat(string(value), 64)
		return finiteDecimal(result, err == nil)
	case float64:
		return finiteDecimal(value, true)
	case float32:
		return finiteDecimal(float64(value), true)
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint64:
		return float64(value), true
	case string:
		trimmed := strings.TrimSpace(value)
		trimmed = strings.TrimSuffix(trimmed, "%")
		trimmed = strings.TrimSuffix(trimmed, "x")
		result, err := strconv.ParseFloat(trimmed, 64)
		return finiteDecimal(result, err == nil)
	default:
		return 0, false
	}
}

func finiteDecimal(value float64, valid bool) (float64, bool) {
	if !valid || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func propertyValue(properties map[string]any, name string) (any, bool) {
	raw, ok := properties[name]
	if !ok {
		return nil, false
	}
	property := object(raw)
	if property == nil {
		return raw, true
	}
	value, ok := property["value"]
	return value, ok
}

func propertySource(properties map[string]any, name string) string {
	property := object(properties[name])
	if property == nil {
		return ""
	}
	source := property["source"]
	if sourceObject := object(source); sourceObject != nil {
		typeName := strings.ToLower(text(sourceObject["type"]))
		data := text(sourceObject["data"])
		if data != "" && data != "-" && typeName == "inherited" {
			return typeName + ":" + data
		}
		return typeName
	}
	return strings.ToLower(text(source))
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func normalizedState(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "HEALTHY" {
		return "ONLINE"
	}
	return value
}

func parseFlexibleTime(value any) time.Time {
	if seconds, ok := unsigned(value); ok && seconds > 0 {
		return time.Unix(int64(seconds), 0).UTC()
	}
	input := text(value)
	if input == "" || input == "-" {
		return time.Time{}
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, format := range formats {
		if parsed, err := time.Parse(format, input); err == nil {
			return parsed
		}
	}
	// ctime output from zpool status contains no zone. It is rendered in the
	// process's local zone, so preserve that interpretation instead of silently
	// treating local wall-clock time as UTC.
	for _, format := range []string{"Mon Jan _2 15:04:05 2006", "Mon Jan 2 15:04:05 2006"} {
		if parsed, err := time.ParseInLocation(format, input, time.Local); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func classifyError(result Result) string {
	message := strings.ToLower(result.ErrString())
	switch {
	case result.TimedOut, strings.Contains(message, "deadline exceeded"), strings.Contains(message, "timed out"):
		return "timeout"
	case errors.Is(result.Err, os.ErrNotExist), strings.Contains(message, "executable file not found"), strings.Contains(message, "command not found"), strings.Contains(message, "no such file or directory"):
		return "missing-tool"
	case errors.Is(result.Err, os.ErrPermission), strings.Contains(message, "permission denied"), strings.Contains(message, "operation not permitted"):
		return "permission-denied"
	case strings.Contains(message, "unrecognized option"), strings.Contains(message, "unknown option"), strings.Contains(message, "invalid option"):
		return "unsupported"
	default:
		return "command-failed"
	}
}

func (result Result) ErrString() string {
	parts := make([]string, 0, 2)
	if result.Err != nil {
		parts = append(parts, result.Err.Error())
	}
	if stderr := strings.TrimSpace(string(result.Stderr)); stderr != "" {
		parts = append(parts, stderr)
	}
	if len(parts) == 0 && result.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit status %d", result.ExitCode))
	}
	if len(parts) == 0 && result.TimedOut {
		parts = append(parts, "command timed out")
	}
	return strings.Join(parts, ": ")
}

func isNoObjects(result Result) bool {
	if result.TimedOut || result.ExitCode < 0 || result.ExitCode > 1 || (result.Err != nil && result.ExitCode == 0) ||
		errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded) {
		return false
	}
	found := false
	for _, line := range strings.Split(string(result.Stdout)+"\n"+string(result.Stderr), "\n") {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
		case "no pools available", "no datasets available", "no datasets found":
			found = true
		default:
			// A warning or failure beside the no-objects message is not
			// authoritative evidence of an empty storage inventory.
			return false
		}
	}
	return found
}
