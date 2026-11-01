// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	data := `[monitor]
listen = "127.0.0.1:9999"
fast_interval = "30s"
wake_disks = true

[thresholds]
capacity_warning = 75
capacity_critical = 92
`
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddress != "127.0.0.1:9999" || c.FastInterval != 30*time.Second || !c.WakeDisks {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.Thresholds.CapacityWarning != 75 || c.Thresholds.CapacityCritical != 92 {
		t.Fatalf("unexpected thresholds: %+v", c.Thresholds)
	}
}

func TestUnknownSettingFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("wat = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadBayMappingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	bayPath := filepath.Join(t.TempDir(), "bays.json")
	data := "[paths]\nbays = \"" + bayPath + "\"\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.BayMapPath != bayPath {
		t.Fatalf("BayMapPath = %q, want %q", config.BayMapPath, bayPath)
	}
}

func TestUnsafePathsAndNonFiniteThresholdsFail(t *testing.T) {
	tests := []string{
		"[paths]\nlock = \"relative.lock\"\n",
		"[paths]\nlock = \"/run/zpoolpilot/../same\"\n",
		"[paths]\nlock = \"/var/lib/zpoolpilot/state.db\"\n",
		"[thresholds]\ncapacity_warning = NaN\n",
		"[monitor]\nlisten = \"not-an-address\"\n",
	}
	for index, data := range tests {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("case %d accepted unsafe config: %s", index, data)
		}
	}
}

func TestLoadRejectsConfigurationSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.toml")
	link := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(target, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("configuration symlink was accepted")
	}
}

func TestPrivilegedPathsAreConfinedToApplicationTrees(t *testing.T) {
	defaults := Defaults()
	if err := validatePrivilegedPaths(defaults); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"audit", func(c *Config) { c.AuditPath = "/root/.bashrc" }},
		{"state", func(c *Config) { c.StatePath = "/etc/shadow" }},
		{"snapshot", func(c *Config) { c.SnapshotPath = "/tmp/latest.json" }},
		{"bays", func(c *Config) { c.BayMapPath = "/var/lib/zpoolpilot/bays.json" }},
		{"lock", func(c *Config) { c.LockPath = "/tmp/apply.lock" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := defaults
			test.mutate(&config)
			if err := validatePrivilegedPaths(config); err == nil {
				t.Fatalf("unsafe privileged %s path was accepted", test.name)
			}
		})
	}
}

func TestLoadPrivilegedRejectsUntrustedDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o770); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivileged(path); err == nil {
		t.Fatal("configuration beneath a non-root or mutable temporary directory was accepted")
	}
}

func TestPrivilegedConfigDirectoryChecksWholeAncestorChain(t *testing.T) {
	if err := validatePrivilegedConfigDirectory(os.TempDir()); err == nil {
		t.Fatal("world-writable configuration ancestor was accepted")
	}
}

func TestLoadRejectsDuplicateSettingsAndUnknownSections(t *testing.T) {
	for _, data := range []string{
		"[monitor]\nwake_disks = false\nwake_disks = true\n",
		"monitor.wake_disks = false\n[monitor]\nwake_disks = true\n",
		"[moniter]\n",
		"[]\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("ambiguous configuration accepted: %s", data)
		}
	}
}

func TestLoadPreservesCommentsInsideEscapedQuotedStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `[paths]
snapshot = "/tmp/snapshot\"#name.json" # trailing comment
state = "/tmp/state\\" # trailing comment after escaped backslash
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotPath != "/tmp/snapshot\"#name.json" || got.StatePath != "/tmp/state\\" {
		t.Fatalf("escaped strings were not preserved: %+v", got)
	}
}

func TestValidateRejectsDerivedStateAndLockPathCollisions(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.SnapshotPath = c.StatePath + ".json" },
		func(c *Config) { c.CollectionLockPath = c.StatePath + ".json.lock" },
		func(c *Config) { c.AuditPath = c.BayMapPath + ".lock" },
	} {
		config := Defaults()
		mutate(&config)
		if err := validate(config); err == nil {
			t.Errorf("derived path collision accepted: %+v", config)
		}
	}
}
