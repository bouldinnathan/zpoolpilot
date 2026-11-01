// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package discovery collects a read-only, point-in-time view of a Linux ZFS
// host.  All operating-system commands are routed through Runner, which keeps
// the package usable by both the CLI and deterministic fixture tests.
package discovery

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

// CapabilityObserve is attached to every command issued by this package.  A
// policy-enforcing runner can therefore prove that discovery never requests a
// mutating capability.
const CapabilityObserve = "observe"

// Command is an argv-only subprocess request.  Program is never a shell and
// Args are never joined into a command string.
type Command struct {
	Program    string
	Args       []string
	Capability string
	Timeout    time.Duration
}

// Result is the complete result of one command invocation.  Non-zero exit
// codes are data, not necessarily Go errors (smartctl notably uses a bitmask).
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	TimedOut bool
	Err      error
}

// Runner executes one argv-only command.  Implementations should pass a safe,
// deterministic environment such as LC_ALL=C and PATH containing only trusted
// system directories.
type Runner interface {
	Run(context.Context, Command) Result
}

// FileSystem is the small read-only filesystem surface needed by discovery.
// EvalSymlinks is used to correlate /dev/disk/by-id links with lsblk devices.
type FileSystem interface {
	ReadFile(string) ([]byte, error)
	ReadDir(string) ([]fs.DirEntry, error)
	EvalSymlinks(string) (string, error)
}

type osFileSystem struct{}

func (osFileSystem) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFileSystem) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFileSystem) EvalSymlinks(name string) (string, error) {
	return filepath.EvalSymlinks(name)
}

// Thresholds controls the deliberately small set of opinionated health
// findings produced during collection.  Zero values use DefaultThresholds.
type Thresholds struct {
	CapacityWarningPercent       float64
	CapacityCriticalPercent      float64
	WearWarningPercent           float64
	WearCriticalPercent          float64
	TemperatureWarningC          float64
	TemperatureCriticalC         float64
	FragmentationAdvisoryPercent float64
}

// DefaultThresholds returns the v1 monitoring defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{
		CapacityWarningPercent:       80,
		CapacityCriticalPercent:      90,
		WearWarningPercent:           80,
		WearCriticalPercent:          90,
		TemperatureWarningC:          55,
		TemperatureCriticalC:         65,
		FragmentationAdvisoryPercent: 70,
	}
}

// Options controls one collection.  The zero value is safe and useful:
// commands time out, sleeping disks are not woken, SMART is collected, and
// health thresholds use DefaultThresholds.
type Options struct {
	CommandTimeout      time.Duration
	IncludeCommandTrace bool
	SkipSMART           bool
	WakeSleepingDisks   bool
	Thresholds          Thresholds

	// Paths are injectable primarily for tests and non-standard Linux layouts.
	MachineIDPath     string
	OSReleasePath     string
	KernelReleasePath string
	MountInfoPath     string
	ARCStatsPath      string
	ByIDDir           string
	ByPathDir         string
}

func (o Options) withDefaults() Options {
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = 10 * time.Second
	}
	if o.MachineIDPath == "" {
		o.MachineIDPath = "/etc/machine-id"
	}
	if o.OSReleasePath == "" {
		o.OSReleasePath = "/etc/os-release"
	}
	if o.KernelReleasePath == "" {
		o.KernelReleasePath = "/proc/sys/kernel/osrelease"
	}
	if o.MountInfoPath == "" {
		o.MountInfoPath = "/proc/self/mountinfo"
	}
	if o.ARCStatsPath == "" {
		o.ARCStatsPath = "/proc/spl/kstat/zfs/arcstats"
	}
	if o.ByIDDir == "" {
		o.ByIDDir = "/dev/disk/by-id"
	}
	if o.ByPathDir == "" {
		o.ByPathDir = "/dev/disk/by-path"
	}

	d := DefaultThresholds()
	if o.Thresholds.CapacityWarningPercent <= 0 {
		o.Thresholds.CapacityWarningPercent = d.CapacityWarningPercent
	}
	if o.Thresholds.CapacityCriticalPercent <= 0 {
		o.Thresholds.CapacityCriticalPercent = d.CapacityCriticalPercent
	}
	if o.Thresholds.WearWarningPercent <= 0 {
		o.Thresholds.WearWarningPercent = d.WearWarningPercent
	}
	if o.Thresholds.WearCriticalPercent <= 0 {
		o.Thresholds.WearCriticalPercent = d.WearCriticalPercent
	}
	if o.Thresholds.TemperatureWarningC <= 0 {
		o.Thresholds.TemperatureWarningC = d.TemperatureWarningC
	}
	if o.Thresholds.TemperatureCriticalC <= 0 {
		o.Thresholds.TemperatureCriticalC = d.TemperatureCriticalC
	}
	if o.Thresholds.FragmentationAdvisoryPercent <= 0 {
		o.Thresholds.FragmentationAdvisoryPercent = d.FragmentationAdvisoryPercent
	}
	return o
}

// CollectorOption replaces an environmental dependency without expanding the
// public collection Options with callbacks.
type CollectorOption func(*Collector)

// WithFileSystem replaces read-only filesystem access.
func WithFileSystem(fsys FileSystem) CollectorOption {
	return func(c *Collector) {
		if fsys != nil {
			c.fs = fsys
		}
	}
}

// WithClock replaces the wall clock, primarily for deterministic tests.
func WithClock(now func() time.Time) CollectorOption {
	return func(c *Collector) {
		if now != nil {
			c.now = now
		}
	}
}

// WithHostname replaces hostname lookup, primarily for deterministic tests.
func WithHostname(hostname func() (string, error)) CollectorOption {
	return func(c *Collector) {
		if hostname != nil {
			c.hostname = hostname
		}
	}
}

// Collector owns the dependencies used to build a Snapshot.
type Collector struct {
	runner   Runner
	fs       FileSystem
	now      func() time.Time
	hostname func() (string, error)
}

// New creates a read-only collector.
func New(runner Runner, options ...CollectorOption) *Collector {
	c := &Collector{
		runner:   runner,
		fs:       osFileSystem{},
		now:      time.Now,
		hostname: os.Hostname,
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// Collect is a convenience function for callers that do not need dependency
// injection beyond Runner.
func Collect(ctx context.Context, runner Runner, options Options) (domain.Snapshot, error) {
	return New(runner).Collect(ctx, options)
}
