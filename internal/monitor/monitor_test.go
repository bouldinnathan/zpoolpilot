// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func sample() domain.Snapshot {
	temp, wear, hours, passed := 42.0, 8.0, uint64(1234), true
	trimSupported := true
	trimTime := time.Unix(1788436800, 0).UTC()
	reallocated, pending, offline, crc := uint64(1), uint64(2), uint64(3), uint64(4)
	return domain.Snapshot{
		APIVersion: domain.SnapshotAPIVersion, Generated: time.Now().UTC(),
		Host: domain.Host{Hostname: "pve1", OpenZFSVersion: "2.3.4"},
		Pools: []domain.Pool{{Name: "tank", GUID: "42", State: "ONLINE", SizeBytes: 1000, FreeBytes: 700, CapacityPercent: 30,
			IO:   domain.IOStats{ReadOperationsPerSecond: 10, WriteOperationsPerSecond: 20, ReadBytesPerSecond: 30, WriteBytesPerSecond: 40},
			Scan: domain.Scan{Function: "scrub", State: "scanning", Percent: 12.5, BytesExamined: 125, BytesTotal: 1000},
			Trim: domain.TrimStats{State: "active", BytesDone: 400, BytesTotal: 1000, Percent: 40, Errors: 1, Supported: &trimSupported, ActionTime: trimTime},
			VDevs: []domain.VDev{{Name: "mirror-0", GUID: "m1", Type: "mirror", Class: "data", State: "ONLINE", SizeBytes: 1000,
				Children: []domain.VDev{{Name: "disk-a", GUID: "d1", Type: "disk", Class: "data", State: "ONLINE", StableDriveID: "wwn-1", Trim: domain.TrimStats{State: "active", BytesDone: 400, BytesTotal: 1000, Percent: 40, Supported: &trimSupported, ActionTime: trimTime}}}}},
		}},
		Datasets: []domain.Dataset{{Name: "tank/vmdata", Type: "filesystem", UsedBytes: 300, AvailableBytes: 650,
			Properties:      map[string]string{"compression": "zstd", "dedup": "off", "keylocation": "file:///secret/path"},
			PropertySources: map[string]string{"compression": "local", "dedup": "default", "keylocation": "local"}}},
		Drives: []domain.Drive{{ID: "wwn-1", Serial: "SN1", Model: "disk\"x", CapacityBytes: 1000, InUse: true,
			Bay: domain.Bay{Label: "A1", Confidence: "verified"}, Health: domain.DriveHealth{Severity: domain.SeverityOK,
				TemperatureC: &temp, PercentageUsed: &wear, PowerOnHours: &hours, SMARTPassed: &passed,
				Reallocated: &reallocated, Pending: &pending, OfflineUncorrect: &offline, CRCErrorCount: &crc}}},
	}
}

func TestLoadRejectsTrailingAndOversizedCache(t *testing.T) {
	directory := t.TempDir()
	trailing := filepath.Join(directory, "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"api_version":"github.com/bouldinnathan/zpoolpilot/v1"}{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(trailing); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
	oversized := filepath.Join(directory, "oversized.json")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSnapshotBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(oversized); err == nil {
		t.Fatal("oversized snapshot cache was accepted")
	}
	symlink := filepath.Join(directory, "snapshot-link.json")
	if err := os.Symlink(trailing, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(symlink); err == nil {
		t.Fatal("snapshot cache symlink was accepted")
	}
}

func TestValidateSnapshotRejectsUnsupportedAPIVersion(t *testing.T) {
	t.Parallel()
	snapshot := sample()
	snapshot.APIVersion = "github.com/bouldinnathan/zpoolpilot/v999"
	if err := ValidateSnapshot(snapshot, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "api_version") {
		t.Fatalf("unsupported cache envelope accepted: %v", err)
	}
}

func TestPrometheusEscapePreventsLineInjection(t *testing.T) {
	got := escape("line\rbreak\nquote\"slash\\tab\t")
	if strings.ContainsRune(got, '\r') || strings.ContainsRune(got, '\n') || !strings.Contains(got, `\n`) {
		t.Fatalf("unsafe escaped label %q", got)
	}
}

func TestPrometheus(t *testing.T) {
	var b bytes.Buffer
	if err := WritePrometheus(&b, sample()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"zpoolpilot_pool_free_bytes", `serial="SN1"`, `model="disk\"x"`, "zpoolpilot_arc_hits",
		`zpoolpilot_dataset_setting{dataset="tank/vmdata",property="compression",source="local",type="filesystem",value="zstd"} 1`,
		"zpoolpilot_vdev_info", `drive_id="wwn-1"`, "zpoolpilot_drive_capacity_bytes",
		"zpoolpilot_drive_pending_sectors", "zpoolpilot_pool_scan_bytes_examined",
		"zpoolpilot_pool_read_operations_per_second", "zpoolpilot_pool_write_bytes_per_second",
		`zpoolpilot_pool_trim_info{guid="42",pool="tank",state="active",supported="true"} 1`,
		"zpoolpilot_pool_trim_bytes_done", `trim_state="active"`, "zpoolpilot_vdev_trim_percent",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret/path") || strings.Contains(out, `property="keylocation"`) {
		t.Fatalf("key location must not be exported in metrics:\n%s", out)
	}
}

func TestPrometheusOmitsSamplesForExactSourceErrors(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		omitted  []string
		retained string
	}{
		{
			name: "arcstats", source: "arcstats",
			omitted:  []string{"arc_size_bytes", "arc_target_bytes", "arc_max_bytes", "arc_hits", "arc_misses", "arc_hit_ratio", "l2arc_size_bytes", "l2arc_hits", "l2arc_misses"},
			retained: "pool_size_bytes",
		},
		{
			name: "pool list", source: "zpool.list",
			omitted:  []string{"pool_size_bytes", "pool_allocated_bytes", "pool_free_bytes", "pool_capacity_percent", "pool_fragmentation_percent"},
			retained: "pool_read_errors",
		},
		{
			name: "pool iostat", source: "zpool.iostat",
			omitted:  []string{"pool_read_operations_per_second", "pool_write_operations_per_second", "pool_read_bytes_per_second", "pool_write_bytes_per_second"},
			retained: "pool_size_bytes",
		},
		{
			name: "pool status", source: "zpool.status",
			omitted:  []string{"pool_read_errors", "pool_write_errors", "pool_checksum_errors", "pool_scan_percent", "pool_trim_info", "vdev_info", "vdev_size_bytes", "vdev_trim_info"},
			retained: "pool_size_bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := sample()
			snapshot.Errors = []domain.SourceError{{Source: test.source, Kind: "parse", Message: "unavailable", Partial: true}}
			var output bytes.Buffer
			if err := WritePrometheus(&output, snapshot); err != nil {
				t.Fatal(err)
			}
			for _, metric := range test.omitted {
				if prometheusSamplePresent(output.String(), metric) {
					t.Errorf("sample zpoolpilot_%s was emitted for failed source %q", metric, test.source)
				}
			}
			if !prometheusSamplePresent(output.String(), test.retained) {
				t.Errorf("unaffected sample zpoolpilot_%s was omitted for failed source %q", test.retained, test.source)
			}
		})
	}
}

func TestPrometheusRetainsSamplesForJSONFallbackErrors(t *testing.T) {
	tests := []struct {
		source string
		metric string
	}{
		{"zpool.list.json", "pool_size_bytes"},
		{"zpool.iostat.json", "pool_read_operations_per_second"},
		{"zpool.status.json", "vdev_info"},
	}
	for _, test := range tests {
		t.Run(test.source, func(t *testing.T) {
			snapshot := sample()
			snapshot.Errors = []domain.SourceError{{Source: test.source, Kind: "unsupported", Message: "used compatibility fallback", Partial: true}}
			var output bytes.Buffer
			if err := WritePrometheus(&output, snapshot); err != nil {
				t.Fatal(err)
			}
			if !prometheusSamplePresent(output.String(), test.metric) {
				t.Errorf("sample zpoolpilot_%s was omitted for successful %q fallback", test.metric, test.source)
			}
		})
	}
}

func prometheusSamplePresent(output, metric string) bool {
	prefix := "zpoolpilot_" + metric
	for _, line := range strings.Split(output, "\n") {
		if line == prefix || strings.HasPrefix(line, prefix+" ") || strings.HasPrefix(line, prefix+"{") {
			return true
		}
	}
	return false
}

func TestHandlerReadiness(t *testing.T) {
	p := filepath.Join(t.TempDir(), "latest.json")
	if err := SaveAtomic(p, sample()); err != nil {
		t.Fatal(err)
	}
	h := Handler{SnapshotPath: p, MaxAge: time.Minute}
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestHandlerReadinessRejectsInvalidGenerationTime(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		generated time.Time
	}{
		{"zero", time.Time{}},
		{"future", time.Now().UTC().Add(maxFutureSnapshotSkew + time.Hour)},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := sample()
			snapshot.Generated = test.generated
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "latest.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			response := httptest.NewRecorder()
			Handler{SnapshotPath: path, MaxAge: time.Minute}.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("readyz returned %d for %s generation time", response.Code, test.name)
			}
		})
	}
}

func TestHandlerReadinessRejectsFutureSMARTTime(t *testing.T) {
	t.Parallel()
	snapshot := sample()
	snapshot.Drives[0].Health.FreshAt = time.Now().UTC().Add(maxFutureSnapshotSkew + time.Hour)
	path := filepath.Join(t.TempDir(), "latest.json")
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	Handler{SnapshotPath: path, MaxAge: time.Minute}.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz returned %d for future SMART timestamp", response.Code)
	}
}

func TestHandlerReadinessRejectsFreshPartialSnapshot(t *testing.T) {
	t.Parallel()
	snapshot := sample()
	snapshot.Partial = true
	snapshot.Errors = []domain.SourceError{{Source: "arcstats", Kind: "parse", Message: "unavailable", Partial: true}}
	path := filepath.Join(t.TempDir(), "latest.json")
	if err := SaveAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	Handler{SnapshotPath: path, MaxAge: time.Minute}.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz returned %d for fresh partial snapshot", response.Code)
	}
}

func TestWorstSeverityPartial(t *testing.T) {
	s := sample()
	s.Partial = true
	if got := ExitCode(s); got != 3 {
		t.Fatalf("got %d", got)
	}
	s.Findings = []domain.Finding{{Severity: domain.SeverityCritical}}
	if got := ExitCode(s); got != 2 {
		t.Fatalf("critical should win, got %d", got)
	}
}

func TestSaveAtomicRejectsInvalidSnapshotWithoutReplacingCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latest.json")
	if err := SaveAtomic(path, sample()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.Snapshot){
		func(s *domain.Snapshot) { s.APIVersion = "github.com/bouldinnathan/zpoolpilot/v999" },
		func(s *domain.Snapshot) { s.Generated = time.Now().Add(time.Hour) },
		func(s *domain.Snapshot) { s.Host.Hostname = strings.Repeat("x", maxSnapshotStringBytes+1) },
		func(s *domain.Snapshot) { s.Drives[0].Health.FreshAt = time.Now().Add(time.Hour) },
	} {
		snapshot := sample()
		mutate(&snapshot)
		if err := SaveAtomic(path, snapshot); err == nil {
			t.Fatal("invalid snapshot was published")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("invalid snapshot replaced existing cache: %v", err)
		}
	}
}

func TestHandlerRoutesWithoutCacheAndHeadBodies(t *testing.T) {
	h := Handler{SnapshotPath: filepath.Join(t.TempDir(), "missing.json")}
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.status {
			t.Errorf("%s %s: got %d, want %d", test.method, test.path, response.Code, test.status)
		}
	}
	if err := SaveAtomic(h.SnapshotPath, sample()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodHead, path, nil))
		if response.Code != http.StatusOK || response.Body.Len() != 0 {
			t.Errorf("HEAD %s: status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
}

func TestHandlerRefreshesSMARTAgeIndependentlyOfSnapshotReadiness(t *testing.T) {
	snapshot := sample()
	snapshot.Drives[0].Health.FreshAt = time.Now().Add(-time.Hour)
	path := filepath.Join(t.TempDir(), "latest.json")
	if err := SaveAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	h := Handler{SnapshotPath: path, MaxAge: time.Minute, HardwareMaxAge: 30 * time.Minute}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `id="drive:wwn-1:smart-stale"`) ||
		!strings.Contains(response.Body.String(), `zpoolpilot_snapshot_severity{severity="unknown"} 1`) ||
		!strings.Contains(response.Body.String(), `health="unknown"`) || strings.Contains(response.Body.String(), `health="ok"`) {
		t.Fatalf("aged SMART cache remained healthy: %d\n%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("fresh complete snapshot was not ready: %d", response.Code)
	}
}

func TestServeRejectsCanceledContextBeforeListening(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, "invalid-address", "missing.json", time.Minute, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve returned %v, want caller cancellation", err)
	}
}
