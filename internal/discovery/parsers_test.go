// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestParseZpoolStatusJSON(t *testing.T) {
	pools, err := parseZpoolStatusJSON(fixture(t, "zpool_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 {
		t.Fatalf("got %d pools, want 1", len(pools))
	}
	pool := pools[0]
	if pool.Name != "tank" || pool.GUID != "123456789" || pool.State != "DEGRADED" {
		t.Fatalf("unexpected pool identity: %+v", pool)
	}
	if pool.Scan.Function != "scrub" || pool.Scan.State != "scanning" || pool.Scan.Percent != 25 || pool.Scan.Errors != 1 {
		t.Errorf("unexpected scan: %+v", pool.Scan)
	}
	if len(pool.VDevs) != 2 {
		t.Fatalf("got %d top-level vdevs, want data mirror and cache", len(pool.VDevs))
	}
	var mirror, cache *domain.VDev
	for index := range pool.VDevs {
		switch pool.VDevs[index].Class {
		case "data":
			mirror = &pool.VDevs[index]
		case "cache":
			cache = &pool.VDevs[index]
		}
	}
	if mirror == nil || mirror.Type != "mirror" || len(mirror.Children) != 2 {
		t.Fatalf("data mirror was not retained: %+v", pool.VDevs)
	}
	if mirror.Children[1].State != "FAULTED" || mirror.Children[1].Path != "/dev/sdb1" {
		t.Errorf("faulted leaf was not parsed: %+v", mirror.Children[1])
	}
	if mirror.Children[0].Trim.State != "active" || mirror.Children[0].Trim.BytesDone != 250 ||
		mirror.Children[0].Trim.BytesTotal != 1000 || mirror.Children[0].Trim.Percent != 25 ||
		mirror.Children[0].Trim.Supported == nil || !*mirror.Children[0].Trim.Supported || mirror.Children[0].Trim.ActionTime.IsZero() {
		t.Errorf("leaf TRIM fields were not parsed: %+v", mirror.Children[0].Trim)
	}
	if pool.Trim.State != "mixed" || pool.Trim.Supported != nil || pool.Trim.BytesDone != 750 || pool.Trim.BytesTotal != 2000 || pool.Trim.Errors != 2 {
		t.Errorf("mixed pool TRIM state was not aggregated conservatively: %+v", pool.Trim)
	}
	if cache == nil || cache.Type != "disk" || cache.Path != "/dev/nvme0n1" {
		t.Errorf("cache class was not parsed: %+v", cache)
	} else if cache.Trim.State != "unsupported" || cache.Trim.Supported == nil || *cache.Trim.Supported {
		t.Errorf("unsupported cache TRIM state was not parsed: %+v", cache.Trim)
	}
}

func TestParseZpoolStatusJSONRejectsIncompleteTopology(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fixture string
		want    string
	}{
		{"zpool_status_invalid_pool.json", "pool \"tank\" is not an object"},
		{"zpool_status_invalid_vdev_container.json", "data vdevs is not an object"},
		{"zpool_status_invalid_vdev.json", "vdev \"disk-a\" is not an object"},
		{"zpool_status_missing_pool_name.json", "missing its name"},
		{"zpool_status_missing_pool_guid.json", "missing its GUID"},
		{"zpool_status_missing_pool_state.json", "missing its state"},
		{"zpool_status_missing_vdev_guid.json", "vdev \"disk-a\" is missing its GUID"},
		{"zpool_status_missing_vdev_state.json", "vdev \"disk-a\" is missing its state"},
		{"zpool_status_missing_leaf_path.json", "leaf disk vdev \"disk-a\" is missing its path"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.fixture, func(t *testing.T) {
			t.Parallel()
			pools, err := parseZpoolStatusJSON(fixture(t, test.fixture))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("pools=%+v, error=%v; want error containing %q", pools, err, test.want)
			}
		})
	}
}

func TestParseZpoolStatusJSONRejectsUnknownScanAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"missing scan", func(pool map[string]any) { delete(pool, "scan_stats") }, "missing scan activity authority"},
		{"scalar scan", func(pool map[string]any) { pool["scan_stats"] = "SCANNING" }, "scan activity is not an object"},
		{"missing state", func(pool map[string]any) { delete(pool["scan_stats"].(map[string]any), "state") }, "missing state"},
		{"unknown state", func(pool map[string]any) { pool["scan_stats"].(map[string]any)["state"] = "FUTURE_STATE" }, "unrecognized state"},
		{"incoherent none", func(pool map[string]any) { pool["scan_stats"].(map[string]any)["function"] = "NONE" }, "inconsistent"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var document map[string]any
			if err := json.Unmarshal(fixture(t, "zpool_status.json"), &document); err != nil {
				t.Fatal(err)
			}
			pool := document["pools"].(map[string]any)["tank"].(map[string]any)
			test.mutate(pool)
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			pools, err := parseZpoolStatusJSON(data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("pools=%+v error=%v; want error containing %q", pools, err, test.want)
			}
		})
	}
}

func TestParseTextScanRequiresExplicitKnownActivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line     string
		function string
		state    string
	}{
		{"none requested", "none", "none"},
		{"scrub in progress since Thu Sep 3 10:00:00 2026", "scrub", "scanning"},
		{"scrub paused since Thu Sep 3 10:00:00 2026", "scrub", "paused"},
		{"scrub repaired 0B in 00:03:12 with 0 errors on Thu Sep 3 10:00:00 2026", "scrub", "finished"},
		{"resilvered 10G in 00:03:12 with 0 errors on Thu Sep 3 10:00:00 2026", "resilver", "finished"},
	}
	for _, test := range tests {
		scan, err := parseTextScan(test.line)
		if err != nil || scan.Function != test.function || scan.State != test.state {
			t.Fatalf("parseTextScan(%q)=%+v,%v; want function=%q state=%q", test.line, scan, err, test.function, test.state)
		}
	}
	if scan, err := parseTextScan("scrub deferred by an unfamiliar condition"); err == nil {
		t.Fatalf("unknown scan authority accepted: %+v", scan)
	}
}

func TestParseZpoolStatusTextCompatibility(t *testing.T) {
	input := `
  pool: tank
 state: DEGRADED
status: One or more devices is unavailable.
action: Replace the faulted device.
  scan: scrub in progress since Wed Sep  2 10:00:00 2026, 37.5% done
config:

	NAME                                      STATE     READ WRITE CKSUM
	tank                                      DEGRADED     0     0     1
	  mirror-0                                DEGRADED     0     0     0
	    /dev/disk/by-id/wwn-a-part1           ONLINE       0     0     0  (42% trimmed, suspended, started at Wed Sep  2 10:00:00 2026)
	    /dev/disk/by-id/wwn-b-part1           FAULTED      3     2     1  (100% trimmed, completed at Wed Sep  2 11:00:00 2026)
	cache
	  /dev/nvme0n1                            ONLINE       0     0     0  (trim unsupported)

errors: No known data errors
`
	pools, err := parseZpoolStatusText([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].State != "DEGRADED" || pools[0].Scan.Percent != 37.5 {
		t.Fatalf("unexpected pool: %+v", pools)
	}
	if len(pools[0].VDevs) != 2 || pools[0].VDevs[1].Class != "cache" {
		t.Fatalf("class hierarchy was not retained: %+v", pools[0].VDevs)
	}
	firstLeaf := pools[0].VDevs[0].Children[0]
	if firstLeaf.Trim.State != "suspended" || firstLeaf.Trim.Percent != 42 || firstLeaf.Trim.ActionTime.IsZero() ||
		firstLeaf.Trim.Supported == nil || !*firstLeaf.Trim.Supported {
		t.Fatalf("text TRIM parenthetical was not retained: %+v", firstLeaf.Trim)
	}
	if pools[0].Trim.State != "mixed" || pools[0].Trim.Supported != nil {
		t.Fatalf("text pool TRIM state should be mixed: %+v", pools[0].Trim)
	}
}

func TestAggregatePoolTrimRequiresConsistentKnownLeaves(t *testing.T) {
	t.Parallel()
	supported := true
	unsupported := false
	tests := []struct {
		name   string
		leaves []domain.VDev
		state  string
		known  *bool
	}{
		{"all active", []domain.VDev{{Trim: domain.TrimStats{State: "active", Supported: &supported}}, {Trim: domain.TrimStats{State: "active", Supported: &supported}}}, "active", &supported},
		{"active and complete", []domain.VDev{{Trim: domain.TrimStats{State: "active", Supported: &supported}}, {Trim: domain.TrimStats{State: "complete", Supported: &supported}}}, "mixed", &supported},
		{"supported and unsupported", []domain.VDev{{Trim: domain.TrimStats{State: "untrimmed", Supported: &supported}}, {Trim: domain.TrimStats{State: "unsupported", Supported: &unsupported}}}, "mixed", nil},
		{"unknown leaf", []domain.VDev{{Trim: domain.TrimStats{State: "active", Supported: &supported}}, {}}, "unknown", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := aggregatePoolTrim(test.leaves)
			if got.State != test.state {
				t.Fatalf("state=%q, want %q (%+v)", got.State, test.state, got)
			}
			if test.known == nil {
				if got.Supported != nil {
					t.Fatalf("supported=%v, want unknown", *got.Supported)
				}
			} else if got.Supported == nil || *got.Supported != *test.known {
				t.Fatalf("supported=%v, want %v", got.Supported, *test.known)
			}
		})
	}
}

func TestParsePoolAndDatasetProperties(t *testing.T) {
	listed, err := parseZpoolListJSON(fixture(t, "zpool_list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].SizeBytes != 2000 || listed[0].CapacityPercent != 91 || listed[0].FragmentationPct != 72 {
		t.Fatalf("unexpected list values: %+v", listed)
	}
	properties, err := parseZpoolGetJSON(fixture(t, "zpool_get.json"))
	if err != nil {
		t.Fatal(err)
	}
	if properties["tank"]["bootfs"] != "tank/ROOT/default" {
		t.Fatalf("bootfs missing from properties: %#v", properties)
	}
	datasets, err := parseZFSGetJSON(fixture(t, "zfs_get.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(datasets) != 2 {
		t.Fatalf("got %d datasets", len(datasets))
	}
	var dataset domain.Dataset
	for _, candidate := range datasets {
		if candidate.Name == "tank/ROOT/default" {
			dataset = candidate
		}
	}
	if dataset.GUID != "987654321" || dataset.UsedBytes != 1000 || dataset.CompressionRatio != 1.5 {
		t.Fatalf("unexpected dataset: %+v", dataset)
	}
	if dataset.Properties["compression"] != "lz4" || dataset.PropertySources["compression"] != "inherited:tank" {
		t.Errorf("property/source lost: properties=%#v sources=%#v", dataset.Properties, dataset.PropertySources)
	}
}

func TestPropertyJSONParsersRejectIncompleteSuccessfulDocuments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		parse func([]byte) error
		want  string
	}{
		{
			name: "empty zpool list needs corroboration", input: `{"pools":{}}`,
			parse: func(data []byte) error { _, err := parseZpoolListJSON(data); return err },
			want:  "independent no-pools evidence",
		},
		{
			name: "non-object zpool list record", input: `{"pools":{"tank":[]}}`,
			parse: func(data []byte) error { _, err := parseZpoolListJSON(data); return err },
			want:  `pool "tank" is not an object`,
		},
		{
			name: "zpool get missing bootfs", input: `{"pools":{"tank":{"name":"tank","guid":1,"properties":{"guid":{"value":1},"size":{"value":100},"allocated":{"value":10},"free":{"value":90},"fragmentation":{"value":0},"capacity":{"value":10},"health":{"value":"ONLINE"}}}}}`,
			parse: func(data []byte) error { _, err := parseZpoolGetJSON(data); return err },
			want:  `required property "bootfs"`,
		},
		{
			name: "zpool get property missing value", input: `{"pools":{"tank":{"name":"tank","guid":1,"properties":{"guid":{"value":1},"size":{"value":100},"allocated":{"value":10},"free":{"value":90},"fragmentation":{"value":0},"capacity":{"value":10},"health":{"value":"ONLINE"},"bootfs":{}}}}}`,
			parse: func(data []byte) error { _, err := parseZpoolGetJSON(data); return err },
			want:  `property "bootfs" is missing its value`,
		},
		{
			name: "empty zfs get needs corroboration", input: `{"datasets":{}}`,
			parse: func(data []byte) error { _, err := parseZFSGetJSON(data); return err },
			want:  "independent no-datasets evidence",
		},
		{
			name: "filesystem missing mountpoint", input: `{"datasets":{"tank":{"name":"tank","type":"filesystem","guid":2,"properties":{"type":{"value":"filesystem"},"guid":{"value":2}}}}}`,
			parse: func(data []byte) error { _, err := parseZFSGetJSON(data); return err },
			want:  "missing its mountpoint property",
		},
		{
			name: "trailing JSON value", input: `{"pools":{}} {"pools":{}}`,
			parse: func(data []byte) error { _, err := parseZpoolListJSON(data); return err },
			want:  "multiple values",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.parse([]byte(test.input))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want error containing %q", err, test.want)
			}
		})
	}

	if _, err := parseZpoolListJSON([]byte(`{"pools":{}}`)); !errors.Is(err, errEmptyPoolsJSON) {
		t.Fatalf("empty pools error %v does not preserve sentinel", err)
	}
	if _, err := parseZFSGetJSON([]byte(`{"datasets":{}}`)); !errors.Is(err, errEmptyDatasetsJSON) {
		t.Fatalf("empty datasets error %v does not preserve sentinel", err)
	}
}

func TestParseLSBLKAndUse(t *testing.T) {
	drives, err := parseLSBLK(fixture(t, "lsblk.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(drives) != 3 {
		t.Fatalf("got %d drives, want 3", len(drives))
	}
	if drives[0].MajorMinor != "259:0" || drives[0].MediaType != "nvme" {
		t.Fatalf("unexpected NVMe: %+v", drives[0])
	}
	var sata *domain.Drive
	for index := range drives {
		if drives[index].DevNode == "/dev/sda" {
			sata = &drives[index]
		}
	}
	if sata == nil || sata.Rotational == nil || !*sata.Rotational || !sata.InUse || !strings.Contains(sata.Use, "zfs_member") {
		t.Fatalf("unexpected SATA drive: %+v", sata)
	}
	if got := strings.Join(sata.BlockNodes, ","); got != "sda,sda1" {
		t.Fatalf("SATA block nodes = %q, want disk and partition", got)
	}
}

func TestParseLSBLKMarksGenericActiveHolders(t *testing.T) {
	input := []byte(`{"blockdevices":[{"name":"/dev/sda","kname":"sda","type":"disk","size":1000,"mountpoints":[null],"fstype":null,"children":[{"name":"/dev/sda1","kname":"sda1","type":"part","mountpoints":[null],"fstype":"crypto_LUKS","children":[{"name":"/dev/mapper/vault","kname":"dm-0","type":"crypt","mountpoints":[null],"fstype":null}]}]}]}`)
	drives, err := parseLSBLK(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(drives) != 1 || !drives[0].InUse || !strings.Contains(drives[0].Use, "is held by crypt") {
		t.Fatalf("active crypt holder was not retained: %+v", drives)
	}
	if got := strings.Join(drives[0].BlockNodes, ","); got != "dm-0,sda,sda1" {
		t.Fatalf("block nodes = %q, want disk and all lsblk descendants", got)
	}
}

func TestParseLSBLKRejectsIncompleteSafetyTree(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"non-object root", `{"blockdevices":["/dev/sda"]}`, "blockdevices[0] is not an object"},
		{"non-object child", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[null],"fstype":null,"children":[null]}]}`, "children[0] is not an object"},
		{"wrong children type", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[null],"fstype":null,"children":{}}]}`, "children is not an array"},
		{"missing mountpoints", `{"blockdevices":[{"name":"/dev/sda","type":"disk","fstype":null}]}`, "missing its mountpoints array"},
		{"wrong mountpoints type", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":"/","fstype":null}]}`, "mountpoints is not an array"},
		{"wrong mountpoint element", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[{}],"fstype":null}]}`, "mountpoints[0] is not text or null"},
		{"missing filesystem type", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[null]}]}`, "missing its filesystem type"},
		{"wrong filesystem type", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[null],"fstype":{}}]}`, "filesystem type is not text or null"},
		{"missing child type", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[null],"fstype":null,"children":[{"name":"/dev/sda1","mountpoints":["/"],"fstype":"ext4"}]}]}`, "missing its device type"},
		{"missing child name", `{"blockdevices":[{"name":"/dev/sda","type":"disk","mountpoints":[null],"fstype":null,"children":[{"type":"part","mountpoints":["/"],"fstype":"ext4"}]}]}`, "missing its block-device name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseLSBLK([]byte(test.input)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseLSBLK() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseSMARTFamiliesAndBitmask(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		fixture  string
		exitCode int
		check    func(t *testing.T, health domain.DriveHealth)
	}{
		{
			name: "ATA attributes", fixture: "smart_ata.json",
			check: func(t *testing.T, health domain.DriveHealth) {
				if health.SMARTPassed == nil || !*health.SMARTPassed || pointerValue(health.Pending) != 1 || pointerValue(health.Reallocated) != 2 || pointerValue(health.CRCErrorCount) != 7 {
					t.Fatalf("unexpected ATA health: %+v", health)
				}
			},
		},
		{
			name: "SAS health bit", fixture: "smart_sas.json", exitCode: 8,
			check: func(t *testing.T, health domain.DriveHealth) {
				if health.SMARTPassed == nil || *health.SMARTPassed || health.Severity != domain.SeverityCritical || pointerValue(health.MediaErrors) != 5 {
					t.Fatalf("unexpected SAS health: %+v", health)
				}
			},
		},
		{
			name: "NVMe endurance", fixture: "smart_nvme.json",
			check: func(t *testing.T, health domain.DriveHealth) {
				if health.PercentageUsed == nil || *health.PercentageUsed != 92 || pointerValue(health.MediaErrors) != 4 || pointerValue(health.PowerOnHours) != 5000 {
					t.Fatalf("unexpected NVMe health: %+v", health)
				}
			},
		},
		{
			name: "sleeping is not a parse failure", fixture: "smart_sleeping.json", exitCode: 2,
			check: func(t *testing.T, health domain.DriveHealth) {
				if !health.Sleeping || health.Severity != domain.SeverityUnknown || !health.FreshAt.IsZero() {
					t.Fatalf("unexpected sleeping health: %+v", health)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			health, err := parseSMART(fixture(t, test.fixture), test.exitCode, now)
			if err != nil {
				t.Fatal(err)
			}
			if test.name != "sleeping is not a parse failure" && health.FreshAt != now {
				t.Fatalf("freshness = %v, want %v", health.FreshAt, now)
			}
			test.check(t, health)
		})
	}
}

func TestParseSMARTATAWearPercentages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	payload := func(attributes []map[string]any, nvme map[string]any) []byte {
		root := map[string]any{
			"smart_status":         map[string]any{"passed": true},
			"ata_smart_attributes": map[string]any{"table": attributes},
		}
		if nvme != nil {
			root["nvme_smart_health_information_log"] = nvme
		}
		encoded, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	attribute := func(name string, value any) map[string]any {
		return map[string]any{"id": 233, "name": name, "value": value, "raw": map[string]any{"value": 987654}}
	}

	tests := []struct {
		name       string
		attributes []map[string]any
		nvme       map[string]any
		want       *float64
	}{
		{name: "media wearout remaining", attributes: []map[string]any{attribute("Media_Wearout_Indicator", 91)}, want: floatPointer(9)},
		{name: "percent lifetime remain", attributes: []map[string]any{attribute("Percent_Lifetime_Remain", 72)}, want: floatPointer(28)},
		{name: "ssd life left", attributes: []map[string]any{attribute("SSD-Life Left", 64)}, want: floatPointer(36)},
		{name: "remaining lifetime", attributes: []map[string]any{attribute("Remaining_Lifetime", 40)}, want: floatPointer(60)},
		{name: "percentage used", attributes: []map[string]any{attribute("Percentage_Used", 17)}, want: floatPointer(17)},
		{name: "percent lifetime used", attributes: []map[string]any{attribute("Percent_Lifetime_Used", 23)}, want: floatPointer(23)},
		{name: "percentage lifetime used", attributes: []map[string]any{attribute("Percentage Lifetime Used", 31)}, want: floatPointer(31)},
		{name: "standard value wins", attributes: []map[string]any{attribute("SSD_Life_Left", 90)}, nvme: map[string]any{"percentage_used": 37}, want: floatPointer(37)},
		{name: "raw value is never wear", attributes: []map[string]any{{"id": 233, "name": "Media_Wearout_Indicator", "raw": map[string]any{"value": 12}}}},
		{name: "negative remains unknown", attributes: []map[string]any{attribute("Percentage_Used", -1)}},
		{name: "over one hundred remains unknown", attributes: []map[string]any{attribute("Percent_Lifetime_Remain", 101)}},
		{name: "non finite remains unknown", attributes: []map[string]any{attribute("SSD_Life_Left", "NaN")}},
		{name: "invalid standard remains unknown", nvme: map[string]any{"percentage_used": 256}},
		{name: "conflicting vendor values remain unknown", attributes: []map[string]any{attribute("SSD_Life_Left", 90), attribute("Percent_Lifetime_Used", 20)}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			health, err := parseSMART(payload(test.attributes, test.nvme), 0, now)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == nil {
				if health.PercentageUsed != nil {
					t.Fatalf("PercentageUsed = %v, want unknown", *health.PercentageUsed)
				}
				return
			}
			if health.PercentageUsed == nil || *health.PercentageUsed != *test.want {
				t.Fatalf("PercentageUsed = %v, want %v", health.PercentageUsed, *test.want)
			}
		})
	}
}

func TestParseSMARTDoesNotTreatEmptyJSONAsFreshOrHealthy(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	health, err := parseSMART([]byte(`{}`), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if health.Severity != domain.SeverityUnknown || !health.FreshAt.IsZero() || health.Message == "" {
		t.Fatalf("empty SMART JSON was accepted as health data: %+v", health)
	}
}

func TestParseSMARTHealthExitBitIsRecognizedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	health, err := parseSMART([]byte(`{}`), 8, now)
	if err != nil {
		t.Fatal(err)
	}
	if health.Severity != domain.SeverityCritical || health.FreshAt != now {
		t.Fatalf("health exit bit was not retained: %+v", health)
	}
}

func TestParseARCStats(t *testing.T) {
	arc, err := parseARCStats(fixture(t, "arcstats.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if arc.Hits != 800 || arc.Misses != 200 || arc.HitRatio != 0.8 || arc.SizeBytes != 1024 || arc.L2Hits != 75 {
		t.Fatalf("unexpected ARC: %+v", arc)
	}
}

func TestHumanNumber(t *testing.T) {
	tests := map[string]uint64{
		"4096": 4096,
		"1K":   1024,
		"1.5M": 1572864,
		"2GiB": 2147483648,
	}
	for input, want := range tests {
		got, ok := parseHumanNumber(input)
		if !ok || got != want {
			t.Errorf("parseHumanNumber(%q) = %d, %v; want %d, true", input, got, ok, want)
		}
	}
}

func TestUnsignedRejectsFractionalNonFiniteAndOverflowValues(t *testing.T) {
	t.Parallel()
	invalid := []any{
		json.Number("1.5"),
		json.Number("1e3"),
		json.Number("18446744073709551616"),
		float64(1.5),
		math.Inf(1),
		math.NaN(),
		-1,
	}
	for _, value := range invalid {
		if parsed, ok := unsigned(value); ok {
			t.Errorf("unsigned(%v)=%d,true; want rejection", value, parsed)
		}
	}
	if _, ok := parseHumanNumber("18446744073709551616"); ok {
		t.Fatal("human-number overflow was accepted")
	}
}

func TestEvaluateFindings(t *testing.T) {
	used := 95.0
	temperature := 70.0
	reallocated, offline, crc := uint64(2), uint64(1), uint64(7)
	passed := false
	snapshot := domain.Snapshot{
		Partial: true,
		Errors:  []domain.SourceError{{Source: "smart:x", Kind: "permission-denied", Partial: true}},
		Pools:   []domain.Pool{{Name: "tank", State: "DEGRADED", CapacityPercent: 91, FragmentationPct: 80, Trim: domain.TrimStats{State: "complete", Errors: 2}}},
		Drives: []domain.Drive{{ID: "wwn:x", Serial: "X", Health: domain.DriveHealth{SMARTPassed: &passed, PercentageUsed: &used,
			TemperatureC: &temperature, Reallocated: &reallocated, OfflineUncorrect: &offline, CRCErrorCount: &crc}}},
	}
	findings := EvaluateFindings(snapshot, DefaultThresholds())
	ids := make(map[string]bool)
	for _, finding := range findings {
		ids[finding.ID] = true
	}
	for _, id := range []string{"pool:tank:state", "pool:tank:capacity", "pool:tank:fragmentation", "pool:tank:trim-errors", "drive:wwn:x:smart", "drive:wwn:x:wear", "drive:wwn:x:temperature", "drive:wwn:x:reallocated-sectors", "drive:wwn:x:offline-uncorrectable", "drive:wwn:x:crc-errors", "drive:wwn:x:bay-unknown", "collection:partial"} {
		if !ids[id] {
			t.Errorf("missing finding %s in %+v", id, findings)
		}
	}
}
