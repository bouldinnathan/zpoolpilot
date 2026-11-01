// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Thresholds struct {
	CapacityWarning      float64
	CapacityCritical     float64
	WearWarning          float64
	WearCritical         float64
	TemperatureWarningC  float64
	TemperatureCriticalC float64
	Fragmentation        float64
}

type Config struct {
	StatePath          string
	SnapshotPath       string
	BayMapPath         string
	PlanDir            string
	LockPath           string
	CollectionLockPath string
	AuditPath          string
	ListenAddress      string
	FastInterval       time.Duration
	HardwareInterval   time.Duration
	CommandTimeout     time.Duration
	WakeDisks          bool
	IncludeCommands    bool
	Thresholds         Thresholds
}

func Defaults() Config {
	return Config{
		StatePath:          "/var/lib/zpoolpilot/state.db",
		SnapshotPath:       "/var/lib/zpoolpilot/latest.json",
		BayMapPath:         "/etc/zpoolpilot/bays.json",
		PlanDir:            "/run/zpoolpilot/plans",
		LockPath:           "/run/zpoolpilot/apply.lock",
		CollectionLockPath: "/run/zpoolpilot/collect/collect.lock",
		AuditPath:          "/var/log/zpoolpilot/audit.jsonl",
		ListenAddress:      "127.0.0.1:9815",
		FastInterval:       time.Minute,
		HardwareInterval:   15 * time.Minute,
		CommandTimeout:     15 * time.Second,
		WakeDisks:          false,
		IncludeCommands:    false,
		Thresholds: Thresholds{
			CapacityWarning:      80,
			CapacityCritical:     90,
			WearWarning:          80,
			WearCritical:         90,
			TemperatureWarningC:  55,
			TemperatureCriticalC: 65,
			Fragmentation:        70,
		},
	}
}

func Load(path string) (Config, error) {
	return load(path, false)
}

// LoadPrivileged pins and authenticates the configuration before a root
// process accepts any configured write target. It also confines application
// state, locks, plans, and audit output to their dedicated system trees.
func LoadPrivileged(path string) (Config, error) {
	return load(path, true)
}

func load(path string, privileged bool) (Config, error) {
	c := Defaults()
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		if privileged {
			return c, validatePrivilegedPaths(c)
		}
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("open configuration: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return c, fmt.Errorf("stat configuration: %w", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		return c, errors.New("configuration must be a singly linked regular file")
	}
	if privileged {
		if stat.Uid != 0 || stat.Mode&0o022 != 0 {
			return c, errors.New("privileged configuration must be owned by root and not writable by group/other")
		}
		if err := validatePrivilegedConfigDirectory(filepath.Dir(path)); err != nil {
			return c, err
		}
	}

	section := ""
	seenSettings := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		raw := strings.TrimSpace(stripComment(scanner.Text()))
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			section = strings.TrimSpace(raw[1 : len(raw)-1])
			if section != "paths" && section != "monitor" && section != "thresholds" {
				return c, fmt.Errorf("%s:%d: unknown section %q", path, line, section)
			}
			continue
		}
		key, value, ok := strings.Cut(raw, "=")
		if !ok {
			return c, fmt.Errorf("%s:%d: expected key = value", path, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		full := key
		if section != "" {
			full = section + "." + key
		}
		if seenSettings[full] {
			return c, fmt.Errorf("%s:%d: duplicate setting %s", path, line, full)
		}
		seenSettings[full] = true
		if err := set(&c, section, key, value); err != nil {
			return c, fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return c, err
	}
	if err := validate(c); err != nil {
		return c, err
	}
	if privileged {
		if err := validatePrivilegedPaths(c); err != nil {
			return c, err
		}
	}
	return c, nil
}

func validatePrivilegedConfigDirectory(directory string) error {
	directory = filepath.Clean(directory)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve privileged configuration directory: %w", err)
	}
	if resolved != directory {
		return errors.New("privileged configuration directory must not contain symbolic links")
	}
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("stat privileged configuration directory %q: %w", current, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("privileged configuration directory %q and its ancestors must be root-owned and not writable by group/other", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func validatePrivilegedPaths(c Config) error {
	paths := []struct {
		name string
		path string
		root string
	}{
		{"paths.state", c.StatePath, "/var/lib/zpoolpilot"},
		{"paths.snapshot", c.SnapshotPath, "/var/lib/zpoolpilot"},
		{"paths.bays", c.BayMapPath, "/etc/zpoolpilot"},
		{"paths.plans", c.PlanDir, "/run/zpoolpilot"},
		{"paths.lock", c.LockPath, "/run/zpoolpilot"},
		{"paths.collection_lock", c.CollectionLockPath, "/run/zpoolpilot"},
		{"paths.audit", c.AuditPath, "/var/log/zpoolpilot"},
	}
	for _, item := range paths {
		if !strictlyWithin(item.path, item.root) {
			return fmt.Errorf("%s must remain beneath %s for privileged execution", item.name, item.root)
		}
	}
	return nil
}

func strictlyWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func stripComment(s string) string {
	inString := false
	escaped := false
	for i, r := range s {
		if inString && escaped {
			escaped = false
			continue
		}
		if inString && r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
		}
		if r == '#' && !inString {
			return s[:i]
		}
	}
	return s
}

func set(c *Config, section, key, raw string) error {
	full := key
	if section != "" {
		full = section + "." + key
	}
	stringValue := func() (string, error) {
		v, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("%s must be a quoted string", full)
		}
		return v, nil
	}
	floatValue := func() (float64, error) {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be numeric", full)
		}
		return v, nil
	}
	durationValue := func() (time.Duration, error) {
		v, err := stringValue()
		if err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", full, err)
		}
		return d, nil
	}

	switch full {
	case "paths.state":
		v, err := stringValue()
		c.StatePath = v
		return err
	case "paths.snapshot":
		v, err := stringValue()
		c.SnapshotPath = v
		return err
	case "paths.bays":
		v, err := stringValue()
		c.BayMapPath = v
		return err
	case "paths.plans":
		v, err := stringValue()
		c.PlanDir = v
		return err
	case "paths.lock":
		v, err := stringValue()
		c.LockPath = v
		return err
	case "paths.collection_lock":
		v, err := stringValue()
		c.CollectionLockPath = v
		return err
	case "paths.audit":
		v, err := stringValue()
		c.AuditPath = v
		return err
	case "monitor.listen":
		v, err := stringValue()
		c.ListenAddress = v
		return err
	case "monitor.fast_interval":
		v, err := durationValue()
		c.FastInterval = v
		return err
	case "monitor.hardware_interval":
		v, err := durationValue()
		c.HardwareInterval = v
		return err
	case "monitor.command_timeout":
		v, err := durationValue()
		c.CommandTimeout = v
		return err
	case "monitor.wake_disks":
		v, err := strconv.ParseBool(raw)
		c.WakeDisks = v
		return err
	case "monitor.include_command_trace":
		v, err := strconv.ParseBool(raw)
		c.IncludeCommands = v
		return err
	case "thresholds.capacity_warning":
		v, err := floatValue()
		c.Thresholds.CapacityWarning = v
		return err
	case "thresholds.capacity_critical":
		v, err := floatValue()
		c.Thresholds.CapacityCritical = v
		return err
	case "thresholds.wear_warning":
		v, err := floatValue()
		c.Thresholds.WearWarning = v
		return err
	case "thresholds.wear_critical":
		v, err := floatValue()
		c.Thresholds.WearCritical = v
		return err
	case "thresholds.temperature_warning_c":
		v, err := floatValue()
		c.Thresholds.TemperatureWarningC = v
		return err
	case "thresholds.temperature_critical_c":
		v, err := floatValue()
		c.Thresholds.TemperatureCriticalC = v
		return err
	case "thresholds.fragmentation_advisory":
		v, err := floatValue()
		c.Thresholds.Fragmentation = v
		return err
	default:
		return fmt.Errorf("unknown setting %s", full)
	}
}

func validate(c Config) error {
	if c.ListenAddress == "" || c.FastInterval <= 0 || c.HardwareInterval <= 0 || c.CommandTimeout <= 0 {
		return errors.New("monitor address and intervals must be non-empty and positive")
	}
	_, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("monitor.listen must be a host:port address: %w", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("monitor.listen port must be between 1 and 65535")
	}
	paths := map[string]string{
		"paths.state": c.StatePath, "paths.snapshot": c.SnapshotPath, "paths.plans": c.PlanDir,
		"paths.bays": c.BayMapPath, "paths.lock": c.LockPath, "paths.collection_lock": c.CollectionLockPath, "paths.audit": c.AuditPath,
		"fallback state": c.StatePath + ".json", "fallback state lock": c.StatePath + ".json.lock",
		"bay mapping lock": c.BayMapPath + ".lock",
	}
	seenPaths := make(map[string]string, len(paths))
	for name, path := range paths {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
			return fmt.Errorf("%s must be a non-empty, absolute, clean path", name)
		}
		if previous, exists := seenPaths[path]; exists {
			return fmt.Errorf("%s and %s must not use the same path", name, previous)
		}
		seenPaths[path] = name
	}
	for name, value := range map[string]float64{
		"capacity_warning":       c.Thresholds.CapacityWarning,
		"capacity_critical":      c.Thresholds.CapacityCritical,
		"wear_warning":           c.Thresholds.WearWarning,
		"wear_critical":          c.Thresholds.WearCritical,
		"temperature_warning_c":  c.Thresholds.TemperatureWarningC,
		"temperature_critical_c": c.Thresholds.TemperatureCriticalC,
		"fragmentation_advisory": c.Thresholds.Fragmentation,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return fmt.Errorf("threshold %s must be between 0 and 100", name)
		}
	}
	if c.Thresholds.CapacityWarning >= c.Thresholds.CapacityCritical {
		return errors.New("capacity_warning must be lower than capacity_critical")
	}
	if c.Thresholds.WearWarning >= c.Thresholds.WearCritical {
		return errors.New("wear_warning must be lower than wear_critical")
	}
	if c.Thresholds.TemperatureWarningC >= c.Thresholds.TemperatureCriticalC {
		return errors.New("temperature_warning_c must be lower than temperature_critical_c")
	}
	return nil
}
