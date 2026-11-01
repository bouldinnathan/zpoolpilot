// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

const (
	maxSnapshotBytes      = 64 << 20
	maxFutureSnapshotSkew = time.Minute
)

func SaveAtomic(path string, snapshot domain.Snapshot) error {
	if snapshot.APIVersion == "" {
		snapshot.APIVersion = domain.SnapshotAPIVersion
	}
	if snapshot.Generated.IsZero() {
		snapshot.Generated = time.Now().UTC()
	}
	if err := ValidateSnapshot(snapshot, time.Now().UTC()); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxSnapshotBytes {
		return fmt.Errorf("snapshot cache exceeds %d bytes", maxSnapshotBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func Load(path string) (domain.Snapshot, error) {
	var snapshot domain.Snapshot
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return snapshot, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return snapshot, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		return snapshot, errors.New("snapshot cache is not a singly linked regular file")
	}
	if info.Size() > maxSnapshotBytes {
		return snapshot, fmt.Errorf("snapshot cache exceeds %d bytes", maxSnapshotBytes)
	}
	decoder := json.NewDecoder(bufio.NewReader(io.LimitReader(file, maxSnapshotBytes+1)))
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return snapshot, fmt.Errorf("decode snapshot cache: %w", err)
	}
	if err := ValidateSnapshot(snapshot, time.Now().UTC()); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// ValidateSnapshot applies the cache-envelope checks shared by the atomic
// JSON cache and the database fallback.
func ValidateSnapshot(snapshot domain.Snapshot, now time.Time) error {
	if err := validateSnapshotText("api_version", snapshot.APIVersion); err != nil {
		return err
	}
	if snapshot.APIVersion != domain.SnapshotAPIVersion {
		return fmt.Errorf("unsupported snapshot api_version %q", snapshot.APIVersion)
	}
	if err := validateSnapshotEnvelope(snapshot); err != nil {
		return err
	}
	return ValidateTimestamps(snapshot, now)
}

// ValidateTimestamps rejects cache times that could otherwise remain fresh
// indefinitely after tampering or a severe clock discontinuity.
func ValidateTimestamps(snapshot domain.Snapshot, now time.Time) error {
	if snapshot.Generated.IsZero() {
		return errors.New("snapshot cache has no generation time")
	}
	if snapshot.Generated.After(now.Add(maxFutureSnapshotSkew)) {
		return errors.New("snapshot cache generation time is implausibly far in the future")
	}
	for _, drive := range snapshot.Drives {
		if !drive.Health.FreshAt.IsZero() && drive.Health.FreshAt.After(now.Add(maxFutureSnapshotSkew)) {
			return fmt.Errorf("snapshot cache SMART time for drive %q is implausibly far in the future", drive.ID)
		}
	}
	return nil
}

func WorstSeverity(snapshot domain.Snapshot) domain.Severity {
	worst := domain.SeverityOK
	for _, finding := range snapshot.Findings {
		if severityRank(finding.Severity) > severityRank(worst) {
			worst = finding.Severity
		}
	}
	if snapshot.Partial && severityRank(domain.SeverityUnknown) > severityRank(worst) {
		worst = domain.SeverityUnknown
	}
	return worst
}

// DriveSeverity combines the last hardware assessment with current findings,
// including collection freshness, without modifying historical health data.
func DriveSeverity(snapshot domain.Snapshot, drive domain.Drive) domain.Severity {
	worst := drive.Health.Severity
	if worst != domain.SeverityOK && severityRank(worst) == 0 {
		worst = domain.SeverityUnknown
	}
	for _, finding := range snapshot.Findings {
		if finding.Scope == "drive/"+drive.ID && severityRank(finding.Severity) > severityRank(worst) {
			worst = finding.Severity
		}
	}
	return worst
}

func ExitCode(snapshot domain.Snapshot) int {
	switch WorstSeverity(snapshot) {
	case domain.SeverityWarning:
		return 1
	case domain.SeverityCritical:
		return 2
	case domain.SeverityUnknown:
		return 3
	default:
		return 0
	}
}

func severityRank(s domain.Severity) int {
	switch s {
	case domain.SeverityCritical:
		return 4
	case domain.SeverityUnknown:
		return 3
	case domain.SeverityWarning:
		return 2
	case domain.SeverityInfo:
		return 1
	default:
		return 0
	}
}

func WritePrometheus(w io.Writer, snapshot domain.Snapshot) error {
	p := &promWriter{w: w}
	failedSources := make(map[string]struct{}, len(snapshot.Errors))
	for _, sourceError := range snapshot.Errors {
		failedSources[sourceError.Source] = struct{}{}
	}
	hasSourceError := func(source string) bool {
		_, failed := failedSources[source]
		return failed
	}
	p.help("host_info", "Static host and ZFS implementation information.", "gauge")
	p.metric("host_info", labels{
		"hostname": snapshot.Host.Hostname, "machine_id": snapshot.Host.MachineID,
		"os": snapshot.Host.OS, "kernel": snapshot.Host.Kernel,
		"openzfs_version": snapshot.Host.OpenZFSVersion,
		"proxmox_version": snapshot.Host.ProxmoxVersion, "proxmox_boot_mode": snapshot.Host.ProxmoxBootMode,
	}, 1)
	p.help("snapshot_timestamp_seconds", "Unix timestamp of the latest complete or partial snapshot.", "gauge")
	p.metric("snapshot_timestamp_seconds", nil, float64(snapshot.Generated.Unix()))
	p.help("snapshot_severity", "Current overall snapshot severity as a one-hot label.", "gauge")
	worst := WorstSeverity(snapshot)
	for _, severity := range []domain.Severity{domain.SeverityOK, domain.SeverityInfo, domain.SeverityWarning, domain.SeverityCritical, domain.SeverityUnknown} {
		p.metric("snapshot_severity", labels{"severity": string(severity)}, boolFloat(severity == worst))
	}
	p.help("collection_partial", "Whether one or more snapshot sources failed.", "gauge")
	p.metric("collection_partial", nil, boolFloat(snapshot.Partial))
	p.help("collection_source_errors", "Number of source collection errors by source and kind.", "gauge")
	counts := map[string]int{}
	for _, e := range snapshot.Errors {
		counts[e.Source+"\x00"+e.Kind]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "\x00", 2)
		p.metric("collection_source_errors", labels{"source": parts[0], "kind": parts[1]}, float64(counts[k]))
	}

	p.help("pool_info", "Static ZFS pool information.", "gauge")
	p.help("pool_size_bytes", "Raw size of a ZFS pool.", "gauge")
	p.help("pool_allocated_bytes", "Raw allocated bytes in a ZFS pool.", "gauge")
	p.help("pool_free_bytes", "Raw free bytes in a ZFS pool; dataset available may differ.", "gauge")
	p.help("pool_capacity_percent", "Raw pool capacity percentage.", "gauge")
	p.help("pool_fragmentation_percent", "ZFS allocator fragmentation percentage.", "gauge")
	p.help("pool_read_errors", "Read errors reported by ZFS.", "counter")
	p.help("pool_write_errors", "Write errors reported by ZFS.", "counter")
	p.help("pool_checksum_errors", "Checksum errors reported by ZFS.", "counter")
	p.help("pool_read_operations_per_second", "Pool read operations per second reported by zpool iostat.", "gauge")
	p.help("pool_write_operations_per_second", "Pool write operations per second reported by zpool iostat.", "gauge")
	p.help("pool_read_bytes_per_second", "Pool read throughput reported by zpool iostat.", "gauge")
	p.help("pool_write_bytes_per_second", "Pool write throughput reported by zpool iostat.", "gauge")
	p.help("pool_scan_percent", "Current scrub, resilver, or expansion progress percentage.", "gauge")
	p.help("pool_scan_bytes_examined", "Bytes examined by the current or latest pool scan.", "gauge")
	p.help("pool_scan_bytes_total", "Total bytes expected for the current or latest pool scan.", "gauge")
	p.help("pool_scan_errors", "Errors reported by the current or latest pool scan.", "gauge")
	p.help("pool_trim_info", "Conservative aggregate on-demand TRIM state and support for a pool.", "gauge")
	p.help("pool_trim_percent", "Conservative aggregate on-demand TRIM progress percentage.", "gauge")
	p.help("pool_trim_bytes_done", "Bytes trimmed by the current or latest on-demand TRIM pass.", "gauge")
	p.help("pool_trim_bytes_total", "Estimated bytes to trim in the current or latest on-demand TRIM pass.", "gauge")
	p.help("pool_trim_errors", "Errors reported by the current or latest on-demand TRIM pass.", "gauge")
	p.help("pool_trim_action_timestamp_seconds", "Start or completion time reported for the pool's latest on-demand TRIM state.", "gauge")
	p.help("vdev_info", "Static ZFS vdev topology and state information.", "gauge")
	p.help("vdev_size_bytes", "Raw size reported for a ZFS vdev.", "gauge")
	p.help("vdev_allocated_bytes", "Raw allocated bytes reported for a ZFS vdev.", "gauge")
	p.help("vdev_read_errors", "Read errors reported for a ZFS vdev.", "counter")
	p.help("vdev_write_errors", "Write errors reported for a ZFS vdev.", "counter")
	p.help("vdev_checksum_errors", "Checksum errors reported for a ZFS vdev.", "counter")
	p.help("vdev_trim_info", "Per-vdev on-demand TRIM state and support from zpool status -t.", "gauge")
	p.help("vdev_trim_percent", "Per-vdev on-demand TRIM progress percentage.", "gauge")
	p.help("vdev_trim_bytes_done", "Per-vdev bytes trimmed.", "gauge")
	p.help("vdev_trim_bytes_total", "Per-vdev estimated bytes to trim.", "gauge")
	p.help("vdev_trim_errors", "Per-vdev on-demand TRIM errors.", "gauge")
	p.help("vdev_trim_action_timestamp_seconds", "Per-vdev on-demand TRIM start or completion timestamp.", "gauge")
	for _, pool := range snapshot.Pools {
		l := labels{"pool": pool.Name, "guid": pool.GUID}
		p.metric("pool_info", merge(l, labels{"state": pool.State, "boot_critical": strconv.FormatBool(pool.BootCritical)}), 1)
		if !hasSourceError("zpool.list") {
			p.metric("pool_size_bytes", l, float64(pool.SizeBytes))
			p.metric("pool_allocated_bytes", l, float64(pool.AllocatedBytes))
			p.metric("pool_free_bytes", l, float64(pool.FreeBytes))
			p.metric("pool_capacity_percent", l, pool.CapacityPercent)
			p.metric("pool_fragmentation_percent", l, pool.FragmentationPct)
		}
		if !hasSourceError("zpool.status") {
			p.metric("pool_read_errors", l, float64(pool.ReadErrors))
			p.metric("pool_write_errors", l, float64(pool.WriteErrors))
			p.metric("pool_checksum_errors", l, float64(pool.ChecksumErrors))
		}
		if !hasSourceError("zpool.iostat") {
			p.metric("pool_read_operations_per_second", l, pool.IO.ReadOperationsPerSecond)
			p.metric("pool_write_operations_per_second", l, pool.IO.WriteOperationsPerSecond)
			p.metric("pool_read_bytes_per_second", l, pool.IO.ReadBytesPerSecond)
			p.metric("pool_write_bytes_per_second", l, pool.IO.WriteBytesPerSecond)
		}
		if !hasSourceError("zpool.status") && pool.Scan.Function != "" {
			scanLabels := merge(l, labels{"function": pool.Scan.Function, "state": pool.Scan.State})
			p.metric("pool_scan_percent", scanLabels, pool.Scan.Percent)
			p.metric("pool_scan_bytes_examined", scanLabels, float64(pool.Scan.BytesExamined))
			p.metric("pool_scan_bytes_total", scanLabels, float64(pool.Scan.BytesTotal))
			p.metric("pool_scan_errors", scanLabels, float64(pool.Scan.Errors))
		}
		if !hasSourceError("zpool.status") && pool.Trim.State != "" {
			trimLabels := merge(l, labels{"state": pool.Trim.State, "supported": trimSupportLabel(pool.Trim.Supported)})
			p.metric("pool_trim_info", trimLabels, 1)
			p.metric("pool_trim_percent", l, pool.Trim.Percent)
			p.metric("pool_trim_bytes_done", l, float64(pool.Trim.BytesDone))
			p.metric("pool_trim_bytes_total", l, float64(pool.Trim.BytesTotal))
			p.metric("pool_trim_errors", l, float64(pool.Trim.Errors))
			if !pool.Trim.ActionTime.IsZero() {
				p.metric("pool_trim_action_timestamp_seconds", l, float64(pool.Trim.ActionTime.Unix()))
			}
		}
		if !hasSourceError("zpool.status") {
			walkVDevs(pool.VDevs, "", func(vdev domain.VDev, parent string) {
				vdevLabels := labels{
					"pool": pool.Name, "pool_guid": pool.GUID, "vdev": vdev.Name,
					"guid": vdev.GUID, "type": vdev.Type, "class": vdev.Class,
					"state": vdev.State, "parent": parent, "drive_id": vdev.StableDriveID,
				}
				p.metric("vdev_info", vdevLabels, 1)
				p.metric("vdev_size_bytes", vdevLabels, float64(vdev.SizeBytes))
				p.metric("vdev_allocated_bytes", vdevLabels, float64(vdev.AllocatedBytes))
				p.metric("vdev_read_errors", vdevLabels, float64(vdev.ReadErrors))
				p.metric("vdev_write_errors", vdevLabels, float64(vdev.WriteErrors))
				p.metric("vdev_checksum_errors", vdevLabels, float64(vdev.ChecksumErrors))
				if vdev.Trim.State != "" {
					trimLabels := merge(vdevLabels, labels{"trim_state": vdev.Trim.State, "trim_supported": trimSupportLabel(vdev.Trim.Supported)})
					p.metric("vdev_trim_info", trimLabels, 1)
					p.metric("vdev_trim_percent", vdevLabels, vdev.Trim.Percent)
					p.metric("vdev_trim_bytes_done", vdevLabels, float64(vdev.Trim.BytesDone))
					p.metric("vdev_trim_bytes_total", vdevLabels, float64(vdev.Trim.BytesTotal))
					p.metric("vdev_trim_errors", vdevLabels, float64(vdev.Trim.Errors))
					if !vdev.Trim.ActionTime.IsZero() {
						p.metric("vdev_trim_action_timestamp_seconds", vdevLabels, float64(vdev.Trim.ActionTime.Unix()))
					}
				}
			})
		}
	}

	p.help("dataset_used_bytes", "Bytes used by a dataset.", "gauge")
	p.help("dataset_available_bytes", "Bytes actually available to a dataset after quotas and reservations.", "gauge")
	p.help("dataset_referenced_bytes", "Bytes referenced by a dataset.", "gauge")
	p.help("dataset_logical_used_bytes", "Logical bytes used before compression and copies.", "gauge")
	p.help("dataset_compression_ratio", "Achieved dataset compression ratio.", "gauge")
	p.help("dataset_setting", "Selected ZFS dataset settings and their inheritance source.", "gauge")
	for _, ds := range snapshot.Datasets {
		l := labels{"dataset": ds.Name, "type": ds.Type}
		p.metric("dataset_used_bytes", l, float64(ds.UsedBytes))
		p.metric("dataset_available_bytes", l, float64(ds.AvailableBytes))
		p.metric("dataset_referenced_bytes", l, float64(ds.ReferencedBytes))
		p.metric("dataset_logical_used_bytes", l, float64(ds.LogicalUsedBytes))
		p.metric("dataset_compression_ratio", l, ds.CompressionRatio)
		for _, property := range monitoredDatasetProperties {
			value, present := ds.Properties[property]
			if !present || value == "" || value == "-" {
				continue
			}
			p.metric("dataset_setting", merge(l, labels{
				"property": property, "value": value, "source": ds.PropertySources[property],
			}), 1)
		}
	}

	p.help("drive_info", "Static drive identity, placement, transport, and health state.", "gauge")
	p.help("drive_temperature_celsius", "Drive temperature reported by SMART/NVMe.", "gauge")
	p.help("drive_percentage_used", "Vendor-normalized SSD/NVMe life percentage used when available.", "gauge")
	p.help("drive_power_on_hours", "Drive power-on hours.", "counter")
	p.help("drive_media_errors", "Media/data-integrity errors reported by the drive.", "counter")
	p.help("drive_smart_passed", "SMART overall-health result: 1 passed, 0 failed; absent when unknown.", "gauge")
	p.help("drive_smart_timestamp_seconds", "Unix timestamp of the drive's most recent successfully parsed health data.", "gauge")
	p.help("drive_capacity_bytes", "Advertised drive capacity in bytes.", "gauge")
	p.help("drive_in_use", "Whether the drive currently has an active or recognized use.", "gauge")
	p.help("drive_sleeping", "Whether SMART collection left the drive in standby and returned no fresh data.", "gauge")
	p.help("drive_health_unsupported", "Whether health collection is unsupported for this drive.", "gauge")
	p.help("drive_reallocated_sectors", "Reallocated sector count when exposed by firmware.", "gauge")
	p.help("drive_pending_sectors", "Pending sector count when exposed by firmware.", "gauge")
	p.help("drive_offline_uncorrectable", "Offline uncorrectable sector count when exposed by firmware.", "gauge")
	p.help("drive_crc_errors", "Drive interface CRC error count when exposed by firmware.", "counter")
	for _, d := range snapshot.Drives {
		l := labels{"drive_id": d.ID, "serial": d.Serial, "model": d.Model, "bay": d.Bay.Label}
		p.metric("drive_info", merge(l, labels{
			"vendor": d.Vendor, "firmware": d.Firmware, "transport": d.Transport,
			"media_type": d.MediaType, "health": string(DriveSeverity(snapshot, d)),
			"bay_enclosure": d.Bay.Enclosure, "bay_source": d.Bay.Source,
			"bay_confidence": d.Bay.Confidence,
		}), 1)
		p.metric("drive_capacity_bytes", l, float64(d.CapacityBytes))
		p.metric("drive_in_use", l, boolFloat(d.InUse))
		p.metric("drive_sleeping", l, boolFloat(d.Health.Sleeping))
		p.metric("drive_health_unsupported", l, boolFloat(d.Health.Unsupported))
		if v := d.Health.TemperatureC; v != nil {
			p.metric("drive_temperature_celsius", l, *v)
		}
		if v := d.Health.PercentageUsed; v != nil {
			p.metric("drive_percentage_used", l, *v)
		}
		if v := d.Health.PowerOnHours; v != nil {
			p.metric("drive_power_on_hours", l, float64(*v))
		}
		if v := d.Health.MediaErrors; v != nil {
			p.metric("drive_media_errors", l, float64(*v))
		}
		if v := d.Health.SMARTPassed; v != nil {
			p.metric("drive_smart_passed", l, boolFloat(*v))
		}
		if !d.Health.FreshAt.IsZero() {
			p.metric("drive_smart_timestamp_seconds", l, float64(d.Health.FreshAt.Unix()))
		}
		if v := d.Health.Reallocated; v != nil {
			p.metric("drive_reallocated_sectors", l, float64(*v))
		}
		if v := d.Health.Pending; v != nil {
			p.metric("drive_pending_sectors", l, float64(*v))
		}
		if v := d.Health.OfflineUncorrect; v != nil {
			p.metric("drive_offline_uncorrectable", l, float64(*v))
		}
		if v := d.Health.CRCErrorCount; v != nil {
			p.metric("drive_crc_errors", l, float64(*v))
		}
	}

	p.help("arc_size_bytes", "Host-wide ARC current size.", "gauge")
	p.help("arc_target_bytes", "Host-wide ARC target size.", "gauge")
	p.help("arc_max_bytes", "Host-wide ARC maximum size.", "gauge")
	p.help("arc_hits", "Host-wide ARC hits.", "counter")
	p.help("arc_misses", "Host-wide ARC misses.", "counter")
	p.help("arc_hit_ratio", "Host-wide ARC hit ratio from zero to one.", "gauge")
	p.help("l2arc_size_bytes", "Host-wide L2ARC current size.", "gauge")
	p.help("l2arc_hits", "Host-wide L2ARC hits.", "counter")
	p.help("l2arc_misses", "Host-wide L2ARC misses.", "counter")
	if !hasSourceError("arcstats") {
		p.metric("arc_size_bytes", nil, float64(snapshot.ARC.SizeBytes))
		p.metric("arc_target_bytes", nil, float64(snapshot.ARC.TargetBytes))
		p.metric("arc_max_bytes", nil, float64(snapshot.ARC.MaxBytes))
		p.metric("arc_hits", nil, float64(snapshot.ARC.Hits))
		p.metric("arc_misses", nil, float64(snapshot.ARC.Misses))
		p.metric("arc_hit_ratio", nil, snapshot.ARC.HitRatio)
		p.metric("l2arc_size_bytes", nil, float64(snapshot.ARC.L2SizeBytes))
		p.metric("l2arc_hits", nil, float64(snapshot.ARC.L2Hits))
		p.metric("l2arc_misses", nil, float64(snapshot.ARC.L2Misses))
	}

	p.help("findings", "Current findings grouped by severity.", "gauge")
	p.help("finding_info", "One metric per current finding, identified by stable id, severity, and scope.", "gauge")
	findings := map[domain.Severity]int{}
	for _, f := range snapshot.Findings {
		findings[f.Severity]++
		p.metric("finding_info", labels{"id": f.ID, "severity": string(f.Severity), "scope": f.Scope}, 1)
	}
	for _, s := range []domain.Severity{domain.SeverityInfo, domain.SeverityWarning, domain.SeverityCritical, domain.SeverityUnknown} {
		p.metric("findings", labels{"severity": string(s)}, float64(findings[s]))
	}
	return p.err
}

func trimSupportLabel(supported *bool) string {
	if supported == nil {
		return "unknown_or_mixed"
	}
	return strconv.FormatBool(*supported)
}

// Keep this list bounded: arbitrary property values can create unbounded label
// cardinality, while these settings are the ones operators commonly need to
// audit across a host. Key locations are intentionally excluded.
var monitoredDatasetProperties = []string{
	"atime", "canmount", "checksum", "compression", "copies", "dedup",
	"encryption", "keyformat", "mountpoint", "primarycache", "quota",
	"readonly", "recordsize", "refquota", "refreservation", "reservation",
	"secondarycache", "sync", "volblocksize", "xattr",
}

func walkVDevs(vdevs []domain.VDev, parent string, visit func(domain.VDev, string)) {
	for _, vdev := range vdevs {
		visit(vdev, parent)
		walkVDevs(vdev.Children, vdev.Name, visit)
	}
}

type Handler struct {
	SnapshotPath   string
	MaxAge         time.Duration
	HardwareMaxAge time.Duration
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "ok\n")
		}
		return
	}
	if r.URL.Path != "/readyz" && r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	snapshot, err := Load(h.SnapshotPath)
	if err != nil {
		http.Error(w, "snapshot unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/readyz" {
		if snapshot.Partial {
			http.Error(w, "snapshot partial", http.StatusServiceUnavailable)
			return
		}
		if h.MaxAge > 0 && time.Since(snapshot.Generated) > h.MaxAge {
			http.Error(w, "snapshot stale", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "ready\n")
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	hardwareMaxAge := h.HardwareMaxAge
	if hardwareMaxAge <= 0 {
		hardwareMaxAge = h.MaxAge
	}
	RefreshTelemetryFindings(&snapshot, time.Now().UTC(), hardwareMaxAge)
	if err := WritePrometheus(w, snapshot); err != nil {
		return
	}
}

func Serve(ctx context.Context, address, snapshotPath string, maxAge, hardwareMaxAge time.Duration) error {
	if ctx == nil {
		return errors.New("nil exporter context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	server := &http.Server{
		Addr: address, Handler: Handler{SnapshotPath: snapshotPath, MaxAge: maxAge, HardwareMaxAge: hardwareMaxAge},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		return errors.Join(ctx.Err(), shutdownErr)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type labels map[string]string
type promWriter struct {
	w   io.Writer
	err error
}

func (p *promWriter) help(name, help, kind string) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, "# HELP zpoolpilot_%s %s\n# TYPE zpoolpilot_%s %s\n", name, help, name, kind)
}
func (p *promWriter) metric(name string, l labels, value float64) {
	if p.err != nil {
		return
	}
	var suffix string
	if len(l) > 0 {
		keys := make([]string, 0, len(l))
		for k := range l {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"=\""+escape(l[k])+"\"")
		}
		suffix = "{" + strings.Join(parts, ",") + "}"
	}
	_, p.err = fmt.Fprintf(p.w, "zpoolpilot_%s%s %s\n", name, suffix, strconv.FormatFloat(value, 'g', -1, 64))
}
func escape(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return strings.ReplaceAll(s, "\"", "\\\"")
}
func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
func merge(a, b labels) labels {
	out := labels{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
