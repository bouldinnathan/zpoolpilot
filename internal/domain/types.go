// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import "time"

const (
	SnapshotAPIVersion = "github.com/bouldinnathan/zpoolpilot/v1"
	PlanAPIVersion     = "github.com/bouldinnathan/zpoolpilot/v1"
)

type Severity string

const (
	SeverityOK       Severity = "ok"
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
	SeverityUnknown  Severity = "unknown"
)

type Risk string

const (
	RiskRead        Risk = "green-read"
	RiskChange      Risk = "yellow-change"
	RiskDestructive Risk = "red-destructive"
)

type SourceError struct {
	Source  string `json:"source"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Partial bool   `json:"partial"`
}

type CommandTrace struct {
	Program      string    `json:"program"`
	Args         []string  `json:"args"`
	Capability   string    `json:"capability"`
	Risk         Risk      `json:"risk"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int64     `json:"duration_ms"`
	ExitCode     int       `json:"exit_code"`
	TimedOut     bool      `json:"timed_out"`
	Error        string    `json:"error,omitempty"`
	OutputHidden bool      `json:"output_hidden"`
}

type Snapshot struct {
	APIVersion string         `json:"api_version"`
	Generated  time.Time      `json:"generated_at"`
	Host       Host           `json:"host"`
	Pools      []Pool         `json:"pools"`
	Datasets   []Dataset      `json:"datasets"`
	Drives     []Drive        `json:"drives"`
	ARC        ARC            `json:"arc"`
	Findings   []Finding      `json:"findings"`
	Errors     []SourceError  `json:"source_errors"`
	Commands   []CommandTrace `json:"command_trace,omitempty"`
	Partial    bool           `json:"partial"`
}

type Host struct {
	Hostname        string `json:"hostname"`
	MachineID       string `json:"machine_id,omitempty"`
	OS              string `json:"os,omitempty"`
	Kernel          string `json:"kernel,omitempty"`
	OpenZFSVersion  string `json:"openzfs_version,omitempty"`
	ProxmoxVersion  string `json:"proxmox_version,omitempty"`
	ProxmoxNode     string `json:"proxmox_node,omitempty"`
	ProxmoxBootMode string `json:"proxmox_boot_mode,omitempty"`
}

type Pool struct {
	Name             string    `json:"name"`
	GUID             string    `json:"guid"`
	State            string    `json:"state"`
	Status           string    `json:"status,omitempty"`
	Action           string    `json:"action,omitempty"`
	BootCritical     bool      `json:"boot_critical"`
	AllocatedBytes   uint64    `json:"allocated_bytes"`
	FreeBytes        uint64    `json:"free_bytes"`
	SizeBytes        uint64    `json:"size_bytes"`
	CapacityPercent  float64   `json:"capacity_percent"`
	FragmentationPct float64   `json:"fragmentation_percent"`
	ReadErrors       uint64    `json:"read_errors"`
	WriteErrors      uint64    `json:"write_errors"`
	ChecksumErrors   uint64    `json:"checksum_errors"`
	Scan             Scan      `json:"scan"`
	Trim             TrimStats `json:"trim"`
	IO               IOStats   `json:"io"`
	VDevs            []VDev    `json:"vdevs"`
}

// IOStats contains the read/write rates reported by zpool iostat. For the
// default one-shot collection these are rates since import, not latency data.
type IOStats struct {
	ReadOperationsPerSecond  float64 `json:"read_operations_per_second"`
	WriteOperationsPerSecond float64 `json:"write_operations_per_second"`
	ReadBytesPerSecond       float64 `json:"read_bytes_per_second"`
	WriteBytesPerSecond      float64 `json:"write_bytes_per_second"`
}

type Scan struct {
	Function      string    `json:"function,omitempty"`
	State         string    `json:"state,omitempty"`
	Percent       float64   `json:"percent,omitempty"`
	BytesExamined uint64    `json:"bytes_examined,omitempty"`
	BytesTotal    uint64    `json:"bytes_total,omitempty"`
	Errors        uint64    `json:"errors,omitempty"`
	EndedAt       time.Time `json:"ended_at,omitempty"`
}

// TrimStats describes on-demand TRIM state. OpenZFS reports this per leaf
// vdev only when `zpool status -t` is requested; Pool.Trim is a conservative
// aggregate and uses state "mixed" or "unknown" rather than guessing.
type TrimStats struct {
	State      string    `json:"state,omitempty"`
	BytesDone  uint64    `json:"bytes_done,omitempty"`
	BytesTotal uint64    `json:"bytes_total,omitempty"`
	Percent    float64   `json:"percent,omitempty"`
	Errors     uint64    `json:"errors,omitempty"`
	Supported  *bool     `json:"supported,omitempty"`
	ActionTime time.Time `json:"action_time,omitempty"`
}

type VDev struct {
	Name           string    `json:"name"`
	GUID           string    `json:"guid,omitempty"`
	Type           string    `json:"type,omitempty"`
	Class          string    `json:"class,omitempty"`
	State          string    `json:"state"`
	Path           string    `json:"path,omitempty"`
	StableDriveID  string    `json:"stable_drive_id,omitempty"`
	ReadErrors     uint64    `json:"read_errors"`
	WriteErrors    uint64    `json:"write_errors"`
	ChecksumErrors uint64    `json:"checksum_errors"`
	Trim           TrimStats `json:"trim"`
	AllocatedBytes uint64    `json:"allocated_bytes,omitempty"`
	SizeBytes      uint64    `json:"size_bytes,omitempty"`
	Children       []VDev    `json:"children,omitempty"`
}

type Dataset struct {
	Name             string            `json:"name"`
	GUID             string            `json:"guid,omitempty"`
	Type             string            `json:"type"`
	UsedBytes        uint64            `json:"used_bytes"`
	AvailableBytes   uint64            `json:"available_bytes"`
	ReferencedBytes  uint64            `json:"referenced_bytes"`
	LogicalUsedBytes uint64            `json:"logical_used_bytes"`
	CompressionRatio float64           `json:"compression_ratio"`
	Properties       map[string]string `json:"properties"`
	PropertySources  map[string]string `json:"property_sources"`
}

type Drive struct {
	ID      string `json:"id"`
	DevNode string `json:"devnode,omitempty"`
	// BlockNodes retains the disk and descendant kernel block names from the
	// current lsblk tree for same-collection sysfs holder checks. It is
	// intentionally absent from snapshots and plan fingerprints.
	BlockNodes          []string    `json:"-"`
	MajorMinor          string      `json:"major_minor,omitempty"`
	ByID                string      `json:"by_id,omitempty"`
	SysfsPath           string      `json:"sysfs_path,omitempty"`
	WWN                 string      `json:"wwn,omitempty"`
	Serial              string      `json:"serial,omitempty"`
	Vendor              string      `json:"vendor,omitempty"`
	Model               string      `json:"model,omitempty"`
	Firmware            string      `json:"firmware,omitempty"`
	CapacityBytes       uint64      `json:"capacity_bytes"`
	LogicalSectorBytes  uint64      `json:"logical_sector_bytes,omitempty"`
	PhysicalSectorBytes uint64      `json:"physical_sector_bytes,omitempty"`
	Transport           string      `json:"transport,omitempty"`
	MediaType           string      `json:"media_type,omitempty"`
	Rotational          *bool       `json:"rotational,omitempty"`
	Bay                 Bay         `json:"bay"`
	Health              DriveHealth `json:"health"`
	InUse               bool        `json:"in_use"`
	Use                 string      `json:"use,omitempty"`
}

type Bay struct {
	Enclosure         string    `json:"enclosure,omitempty"`
	EnclosureIdentity string    `json:"enclosure_identity,omitempty"`
	Label             string    `json:"label,omitempty"`
	Slot              int       `json:"slot"`
	SlotKnown         bool      `json:"slot_known"`
	Source            string    `json:"source,omitempty"`
	Confidence        string    `json:"confidence,omitempty"`
	VerifiedAt        time.Time `json:"verified_at,omitempty"`
}

type DriveHealth struct {
	Severity         Severity  `json:"severity"`
	SMARTPassed      *bool     `json:"smart_passed,omitempty"`
	TemperatureC     *float64  `json:"temperature_c,omitempty"`
	PowerOnHours     *uint64   `json:"power_on_hours,omitempty"`
	PercentageUsed   *float64  `json:"percentage_used,omitempty"`
	MediaErrors      *uint64   `json:"media_errors,omitempty"`
	Reallocated      *uint64   `json:"reallocated_sectors,omitempty"`
	Pending          *uint64   `json:"pending_sectors,omitempty"`
	OfflineUncorrect *uint64   `json:"offline_uncorrectable,omitempty"`
	CRCErrorCount    *uint64   `json:"crc_error_count,omitempty"`
	FreshAt          time.Time `json:"fresh_at,omitempty"`
	Sleeping         bool      `json:"sleeping"`
	Unsupported      bool      `json:"unsupported"`
	Message          string    `json:"message,omitempty"`
}

type ARC struct {
	SizeBytes   uint64  `json:"size_bytes"`
	TargetBytes uint64  `json:"target_bytes"`
	MaxBytes    uint64  `json:"max_bytes"`
	Hits        uint64  `json:"hits"`
	Misses      uint64  `json:"misses"`
	HitRatio    float64 `json:"hit_ratio"`
	L2SizeBytes uint64  `json:"l2_size_bytes"`
	L2Hits      uint64  `json:"l2_hits"`
	L2Misses    uint64  `json:"l2_misses"`
}

type Finding struct {
	ID          string   `json:"id"`
	Severity    Severity `json:"severity"`
	Scope       string   `json:"scope"`
	Title       string   `json:"title"`
	Evidence    string   `json:"evidence,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
}

type ActionPlan struct {
	APIVersion          string           `json:"api_version"`
	Kind                string           `json:"kind"`
	ID                  string           `json:"id"`
	CreatedAt           time.Time        `json:"created_at"`
	ExpiresAt           time.Time        `json:"expires_at"`
	HostMachineID       string           `json:"host_machine_id"`
	Operation           string           `json:"operation"`
	Risk                Risk             `json:"risk"`
	TargetPool          PoolRef          `json:"target_pool,omitempty"`
	TargetVDev          VDevRef          `json:"target_vdev,omitempty"`
	SourceDrives        []DriveRef       `json:"source_drives,omitempty"`
	TargetDrives        []DriveRef       `json:"target_drives,omitempty"`
	Desired             map[string]any   `json:"desired,omitempty"`
	TopologyFingerprint string           `json:"topology_fingerprint"`
	SnapshotFingerprint string           `json:"snapshot_fingerprint"`
	Blockers            []string         `json:"blockers"`
	Warnings            []string         `json:"warnings"`
	ExpectedResult      string           `json:"expected_result"`
	Commands            []PlannedCommand `json:"commands"`
	Confirmation        string           `json:"confirmation"`
}

type PoolRef struct {
	Name string `json:"name"`
	GUID string `json:"guid"`
}

type VDevRef struct {
	Name string `json:"name"`
	GUID string `json:"guid"`
}

type DriveRef struct {
	ID                  string `json:"id"`
	WWN                 string `json:"wwn,omitempty"`
	Serial              string `json:"serial,omitempty"`
	ByID                string `json:"by_id"`
	MajorMinor          string `json:"major_minor,omitempty"`
	CapacityBytes       uint64 `json:"capacity_bytes"`
	LogicalSectorBytes  uint64 `json:"logical_sector_bytes"`
	PhysicalSectorBytes uint64 `json:"physical_sector_bytes"`
	Bay                 string `json:"bay,omitempty"`
}

type PlannedCommand struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
	Risk    Risk     `json:"risk"`
	Reason  string   `json:"reason"`
}
