// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestNVMeEnduranceBeyondRatedLifeRemainsVisible(t *testing.T) {
	for _, used := range []int{100, 101, 254, 255} {
		t.Run(fmt.Sprint(used), func(t *testing.T) {
			data := []byte(fmt.Sprintf(`{"nvme_smart_health_information_log":{"percentage_used":%d}}`, used))
			health, err := parseSMART(data, 0, time.Now())
			if err != nil || health.PercentageUsed == nil || *health.PercentageUsed != float64(used) {
				t.Fatalf("endurance %d disappeared: health=%+v err=%v", used, health, err)
			}
			findings := EvaluateFindings(domain.Snapshot{Drives: []domain.Drive{{ID: "nvme", Health: health}}}, DefaultThresholds())
			for _, finding := range findings {
				if finding.ID == "drive:nvme:wear" && finding.Severity == domain.SeverityCritical {
					return
				}
			}
			t.Fatalf("missing critical endurance finding: %+v", findings)
		})
	}
	for _, used := range []string{"-1", "256", "12.5", `"12.5"`, `"NaN"`} {
		health, err := parseSMART([]byte(`{"nvme_smart_health_information_log":{"percentage_used":`+used+`}}`), 0, time.Now())
		if err != nil || health.PercentageUsed != nil {
			t.Fatalf("invalid NVMe byte %s accepted: health=%+v err=%v", used, health, err)
		}
	}
}

func TestSCSIMediaErrorsRequireUncorrectedCounters(t *testing.T) {
	for _, test := range []struct {
		name string
		log  string
		want *uint64
	}{
		{"corrected only", `{"read":{"total_errors_corrected":999}}`, nil},
		{"uncorrected zero", `{"read":{"total_errors_corrected":999,"total_uncorrected_errors":0}}`, uintPointer(0)},
		{"uncorrected sum", `{"read":{"total_uncorrected_errors":2},"write":{"total_uncorrected_errors":3}}`, uintPointer(5)},
		{"sum saturates", `{"read":{"total_uncorrected_errors":18446744073709551615},"write":{"total_uncorrected_errors":1}}`, uintPointer(math.MaxUint64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			health, err := parseSMART([]byte(`{"scsi_error_counter_log":`+test.log+`}`), 0, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if (health.MediaErrors == nil) != (test.want == nil) || (test.want != nil && *health.MediaErrors != *test.want) {
				t.Fatalf("media errors=%v want=%v", health.MediaErrors, test.want)
			}
		})
	}
}

func TestPartitionCorrelationRequiresExactPartitionSuffix(t *testing.T) {
	for _, test := range []struct {
		disk, candidate string
		want            bool
	}{
		{"/dev/sda", "/dev/sda1", true},
		{"/dev/sda", "/dev/sda0", false},
		{"/dev/sda", "/dev/sda+1", false},
		{"/dev/sda", "/dev/sdaa1", false},
		{"/dev/nvme0n1", "/dev/nvme0n1p2", true},
		{"/dev/nvme0n1", "/dev/nvme0n12", false},
		{"/dev/nvme0n1", "/dev/nvme0n12p1", false},
		{"/dev/mmcblk0", "/dev/mmcblk0p1", true},
		{"/dev/mmcblk0", "/dev/mmcblk01", false},
	} {
		if got := isPartitionOf(test.candidate, test.disk); got != test.want {
			t.Errorf("isPartitionOf(%q, %q)=%v want=%v", test.candidate, test.disk, got, test.want)
		}
	}
	drives := []domain.Drive{{ID: "first", DevNode: "/dev/nvme0n1", ByID: "/dev/disk/by-id/nvme-first"}}
	for _, path := range []string{"/dev/nvme0n12", "/dev/disk/by-id/nvme-first-partX", "/dev/disk/by-id/nvme-first-part1-extra", "/dev/disk/by-id/nvme-first-part0"} {
		if index, ok := matchDrivePath(fakeFileSystem{}, path, drives, nil); ok {
			t.Errorf("unrelated path %q matched drive %d", path, index)
		}
	}
	if _, ok := matchDrivePath(fakeFileSystem{}, "/dev/disk/by-id/nvme-first-part2", drives, nil); !ok {
		t.Fatal("valid by-id partition did not correlate")
	}
	filesystem := fakeFileSystem{symlinks: map[string]string{"/dev/disk/by-id/nvme-first-part2": "/dev/nvme9n1p2"}}
	if _, ok := matchDrivePath(filesystem, "/dev/disk/by-id/nvme-first-part2", drives, nil); ok {
		t.Fatal("by-id filename overrode contradictory live symlink evidence")
	}
}

func TestNonfiniteTelemetryNeverEntersSnapshot(t *testing.T) {
	for _, value := range []any{"NaN", "Inf", "+Inf", "-Inf", math.Inf(1), math.Inf(-1), math.NaN(), float32(math.Inf(1)), json.Number("NaN")} {
		if parsed, ok := decimal(value); ok || parsed != 0 {
			t.Errorf("decimal(%v)=%v,%v; want zero,false", value, parsed, ok)
		}
	}
	health, err := parseSMART([]byte(`{"temperature":{"current":"NaN"}}`), 0, time.Now())
	if err != nil || health.TemperatureC != nil || !health.FreshAt.IsZero() {
		t.Fatalf("nonfinite temperature became a fresh sample: %+v err=%v", health, err)
	}
	if _, err := json.Marshal(domain.Snapshot{Drives: []domain.Drive{{Health: health}}}); err != nil {
		t.Fatalf("invalid telemetry broke snapshot serialization: %v", err)
	}
}

func TestCollectorRejectsInterruptedSMARTDespiteValidJSON(t *testing.T) {
	for _, result := range []Result{
		{TimedOut: true},
		{TimedOut: true, ExitCode: -1, Err: context.DeadlineExceeded},
		{ExitCode: -1, Err: errors.New("signal: terminated")},
		{ExitCode: 8, Err: context.Canceled},
		{Err: errors.New("output capture failed")},
	} {
		result.Stdout = fixture(t, "smart_nvme.json")
		runner := &fakeRunner{responses: map[string]Result{commandKey("smartctl", "--json=c", "--all", "--device", "nvme", "/dev/nvme0n1"): result}}
		snapshot := domain.Snapshot{Generated: time.Now()}
		state := collectionState{collector: New(runner), options: (Options{}).withDefaults(), snapshot: &snapshot}
		drives := []domain.Drive{{ID: "nvme", DevNode: "/dev/nvme0n1", Transport: "nvme"}}
		state.collectSMART(context.Background(), drives)
		state.finish()
		if !snapshot.Partial || len(snapshot.Errors) != 1 || drives[0].Health.Severity != domain.SeverityUnknown || !drives[0].Health.FreshAt.IsZero() {
			t.Fatalf("interrupted SMART became trusted telemetry: result=%+v drive=%+v errors=%+v", result, drives[0], snapshot.Errors)
		}
	}
}

func TestSMARTEmbeddedStatusCannotClearProcessFailure(t *testing.T) {
	health, err := parseSMART([]byte(`{"smartctl":{"exit_status":0},"smart_status":{"passed":true}}`), 8, time.Now())
	if err != nil || health.Severity != domain.SeverityCritical {
		t.Fatalf("embedded status cleared failing process status: health=%+v err=%v", health, err)
	}
}

func TestNoObjectsRequiresUninterruptedUnambiguousEvidence(t *testing.T) {
	for _, result := range []Result{
		{Stderr: []byte("no pools available\n"), ExitCode: 1},
		{Stdout: []byte("no datasets available\n")},
	} {
		if !isNoObjects(result) {
			t.Errorf("valid no-objects response rejected: %+v", result)
		}
	}
	for _, result := range []Result{
		{Stderr: []byte("no pools available\n"), TimedOut: true},
		{Stderr: []byte("no pools available\n"), ExitCode: -1, Err: context.Canceled},
		{Stderr: []byte("no pools available\n"), ExitCode: 1, Err: context.Canceled},
		{Stderr: []byte("permission denied; no pools available\n"), ExitCode: 1},
		{Stderr: []byte("no pools available\npermission denied\n"), ExitCode: 1},
	} {
		if isNoObjects(result) {
			t.Errorf("failed response accepted as empty inventory: %+v", result)
		}
	}
	if successful(Result{TimedOut: true}) {
		t.Fatal("timeout accepted as successful collection")
	}
}

func TestARCRequiresCompleteCounters(t *testing.T) {
	for _, input := range []string{
		"13 1 0x01 7 336 123456789\nname type data\n",
		strings.Replace(string(fixture(t, "arcstats.txt")), "hits                               4 800\n", "", 1),
		strings.Replace(string(fixture(t, "arcstats.txt")), "misses                             4 200", "misses 4 invalid", 1),
	} {
		if arc, err := parseARCStats([]byte(input)); err == nil {
			t.Fatalf("incomplete ARC data accepted as zero counters: %+v", arc)
		}
	}
	input := strings.Replace(string(fixture(t, "arcstats.txt")), "hits                               4 800", "hits 4 18446744073709551615", 1)
	input = strings.Replace(input, "misses                             4 200", "misses 4 18446744073709551615", 1)
	arc, err := parseARCStats([]byte(input))
	if err != nil || arc.HitRatio != 0.5 {
		t.Fatalf("ARC hit ratio overflow: %+v err=%v", arc, err)
	}
}

func TestIOStatRequiresCompleteRatesAndPoolCoverage(t *testing.T) {
	for _, test := range []struct {
		name, input string
	}{
		{"truncated rates", "tank\t100\t900\nbackup\t200\t800\t0\t0\t0\t0\n"},
		{"missing pool", "tank\t100\t900\t1\t2\t3\t4\n"},
		{"invalid rate", "tank\t100\t900\t1\t2\t3\t4\nbackup\t200\t800\tNaN\t0\t0\t0\n"},
		{"negative rate", "tank\t100\t900\t-1\t2\t3\t4\nbackup\t200\t800\t0\t0\t0\t0\n"},
		{"empty despite imports", "no pools available\n"},
		{"duplicate pool", "tank\t100\t900\t1\t2\t3\t4\ntank\t100\t900\t1\t2\t3\t4\n"},
		{"undiscovered pool", "other\t100\t900\t1\t2\t3\t4\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pools := map[string]*domain.Pool{"tank": {Name: "tank"}, "backup": {Name: "backup"}}
			if err := applyIOStatText(pools, []byte(test.input)); err == nil {
				t.Fatal("incomplete iostat accepted")
			}
			for _, pool := range pools {
				if pool.IO != (domain.IOStats{}) || pool.AllocatedBytes != 0 || pool.FreeBytes != 0 {
					t.Fatalf("partial iostat modified pool: %+v", pool)
				}
			}
		})
	}
	pools := map[string]*domain.Pool{"tank": {Name: "tank"}, "backup": {Name: "backup"}}
	if err := applyIOStatText(pools, []byte("tank\t100\t900\t1\t2\t3\t4\nbackup\t200\t800\t0\t0\t0\t0\n")); err != nil {
		t.Fatal(err)
	}
	if pools["tank"].IO.WriteBytesPerSecond != 4 || pools["backup"].IO != (domain.IOStats{}) || pools["backup"].AllocatedBytes != 200 {
		t.Fatalf("complete iostat including genuine zero rates was lost: %+v", pools)
	}
}
