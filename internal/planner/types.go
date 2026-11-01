// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package planner constructs and applies short-lived, revalidated ZFS action
// plans. It is the only package which translates a requested storage change
// into mutation commands.
package planner

import (
	"context"
	"errors"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

const (
	PlanKind       = "ActionPlan"
	DefaultPlanTTL = 15 * time.Minute
	MaxPlanTTL     = 15 * time.Minute
)

type Operation string

const (
	OperationCreatePool      Operation = "create-pool"
	OperationPrepareDisk     Operation = "prepare-disk"
	OperationReplaceDisk     Operation = "replace-disk"
	OperationAttachMirror    Operation = "attach-mirror"
	OperationExpandRAIDZ     Operation = "expand-raidz"
	OperationAddVDev         Operation = "add-vdev"
	OperationAddSpare        Operation = "add-spare"
	OperationReplaceBootDisk Operation = "replace-boot-disk"
	OperationScrubStart      Operation = "scrub-start"
	OperationScrubStop       Operation = "scrub-stop"
	OperationTrimStart       Operation = "trim-start"
	OperationTrimStop        Operation = "trim-stop"
	OperationClearErrors     Operation = "clear-errors"
)

type BootMode string

const (
	BootModeSystemdBoot BootMode = "systemd-boot"
	BootModeGRUB        BootMode = "grub"
	// BootModePlainGRUB exists only so callers can report a precise blocker.
	// This planner never dispatches grub-install.
	BootModePlainGRUB BootMode = "plain-grub"

	ProxmoxESPPartition = 2
	ProxmoxZFSPartition = 3
)

type Topology string

const (
	TopologySingle Topology = "single"
	TopologyMirror Topology = "mirror"
	TopologyRAIDZ1 Topology = "raidz1"
	TopologyRAIDZ2 Topology = "raidz2"
	TopologyRAIDZ3 Topology = "raidz3"
)

var (
	ErrInvalidPlan  = errors.New("invalid action plan")
	ErrPlanExpired  = errors.New("action plan expired")
	ErrPlanBlocked  = errors.New("action plan has blockers")
	ErrPlanChanged  = errors.New("action plan changed after preview")
	ErrRevalidation = errors.New("fresh revalidation failed")
	ErrTTYRequired  = errors.New("apply requires a real TTY")
	ErrConfirmation = errors.New("confirmation rejected")
	ErrLockHeld     = errors.New("another apply is in progress")
	ErrAudit        = errors.New("mutation audit failed")
)

// Builder supplies deterministic time and IDs in tests. Nil fields use secure
// production defaults.
type Builder struct {
	Now   func() time.Time
	TTL   time.Duration
	NewID func() (string, error)
}

type CreatePoolRequest struct {
	Snapshot          domain.Snapshot
	PoolName          string
	Topology          Topology
	Drives            []domain.Drive
	PoolProperties    map[string]string
	DatasetProperties map[string]string
	Expert            bool
}

type PrepareDiskRequest struct {
	Snapshot domain.Snapshot
	Drive    domain.Drive
}

type ReplaceDiskRequest struct {
	Snapshot  domain.Snapshot
	Pool      domain.Pool
	OldVDev   domain.VDev
	NewDrive  domain.Drive
	AllowBoot bool
}

type AttachMirrorRequest struct {
	Snapshot     domain.Snapshot
	Pool         domain.Pool
	ExistingVDev domain.VDev
	NewDrive     domain.Drive
	AllowBoot    bool
}

type ExpandRAIDZRequest struct {
	Snapshot  domain.Snapshot
	Pool      domain.Pool
	RAIDZVDev domain.VDev
	NewDrive  domain.Drive
	AllowBoot bool
}

type AddVDevRequest struct {
	Snapshot  domain.Snapshot
	Pool      domain.Pool
	Topology  Topology
	Drives    []domain.Drive
	AllowBoot bool
}

type AddSpareRequest struct {
	Snapshot  domain.Snapshot
	Pool      domain.Pool
	Drives    []domain.Drive
	AllowBoot bool
}

// ReplaceBootDiskRequest is deliberately separate from ReplaceDiskRequest.
// BootMode must come from an operator-verified proxmox-boot-tool status; the
// planner neither detects nor repairs boot mode.
type ReplaceBootDiskRequest struct {
	Snapshot    domain.Snapshot
	Pool        domain.Pool
	OldVDev     domain.VDev
	SourceDrive domain.Drive
	NewDrive    domain.Drive
	BootMode    BootMode
}

// MaintenanceRequest identifies an already-imported pool for a bounded,
// routine maintenance action. The selected pool is rebound to its discovered
// GUID when the plan is built and again before apply.
type MaintenanceRequest struct {
	Snapshot domain.Snapshot
	Pool     domain.Pool
}

// ConfirmationRequest is handed to the callback immediately before a command.
// Red commands require Response to equal Expected exactly; yellow commands
// accept an empty response (the documented Enter default), y, or yes.
type ConfirmationRequest struct {
	PlanID      string
	Fingerprint string
	Command     domain.PlannedCommand
	Index       int
	Total       int
	Expected    string
}

type ConfirmFunc func(context.Context, ConfirmationRequest) (response string, err error)

type Confirmations struct {
	Green  ConfirmFunc
	Yellow ConfirmFunc
	Red    ConfirmFunc
}

type Unlocker interface {
	Unlock() error
}

type Locker interface {
	Lock(context.Context) (Unlocker, error)
}

type AuditRecord struct {
	Phase       string
	PlanID      string
	Operation   Operation
	Fingerprint string
	At          time.Time
	Before      *domain.Snapshot
	After       *domain.Snapshot
	Commands    []domain.CommandTrace
	Error       string
}

type AuditFunc func(context.Context, AuditRecord) error

// CommandGuard performs last-mile operating-system checks which cannot be
// represented by the topology snapshot (for example, kernel partition
// ownership). Index is one-based and the callback runs after immediate fresh
// revalidation but before the runner is allowed to dispatch.
type CommandGuard func(context.Context, domain.ActionPlan, domain.PlannedCommand, int) error

type ApplyOptions struct {
	Runner         execx.Runner
	Now            func() time.Time
	IsTTY          func() bool
	Locker         Locker
	LockPath       string
	FreshSnapshot  func(context.Context) (domain.Snapshot, error)
	Confirm        Confirmations
	BeforeAudit    AuditFunc
	AfterAudit     AuditFunc
	CommandGuard   CommandGuard
	CommandTimeout time.Duration
}

type ApplyResult struct {
	Fingerprint string
	Before      domain.Snapshot
	After       domain.Snapshot
	Commands    []domain.CommandTrace
}
