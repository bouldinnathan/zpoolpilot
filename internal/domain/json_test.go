// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSnapshotRequiredCollectionsKeepTheirJSONTypes(t *testing.T) {
	for _, value := range []Snapshot{
		{},
		{Pools: []Pool{{Name: "tank"}}, Datasets: []Dataset{{Name: "tank"}}, Commands: []CommandTrace{{Program: "pveversion"}}},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"pools", "datasets", "drives", "findings", "source_errors"} {
			if _, ok := document[field].([]any); !ok {
				t.Errorf("required %s is not an array: %s", field, encoded)
			}
		}
		if len(value.Pools) > 0 {
			pool := document["pools"].([]any)[0].(map[string]any)
			if _, ok := pool["vdevs"].([]any); !ok {
				t.Errorf("required pool vdevs is not an array: %s", encoded)
			}
			dataset := document["datasets"].([]any)[0].(map[string]any)
			for _, field := range []string{"properties", "property_sources"} {
				if _, ok := dataset[field].(map[string]any); !ok {
					t.Errorf("required dataset %s is not an object: %s", field, encoded)
				}
			}
			trace := document["command_trace"].([]any)[0].(map[string]any)
			if _, ok := trace["args"].([]any); !ok {
				t.Errorf("required trace args is not an array: %s", encoded)
			}
			if value.Pools[0].VDevs != nil || value.Datasets[0].Properties != nil || value.Commands[0].Args != nil {
				t.Fatal("marshaling changed caller-owned nested collections")
			}
		}
		if value.Findings != nil || value.Errors != nil || value.Drives != nil {
			t.Fatal("marshaling changed caller-owned snapshot collections")
		}
	}
}

func TestSnapshotPopulatedCollectionsRoundTrip(t *testing.T) {
	value := Snapshot{
		APIVersion: SnapshotAPIVersion, Generated: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
		Pools:    []Pool{{Name: "tank", VDevs: []VDev{{Name: "disk"}}}},
		Datasets: []Dataset{{Name: "tank", Properties: map[string]string{"mountpoint": "/tank"}, PropertySources: map[string]string{"mountpoint": "local"}}},
		Drives:   []Drive{{ID: "disk", Health: DriveHealth{FreshAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}}},
		Findings: []Finding{{ID: "finding"}}, Errors: []SourceError{{Source: "test"}}, Commands: []CommandTrace{{Program: "zpool", Args: []string{"status"}}},
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, got) {
		t.Fatalf("populated snapshot changed during round trip: got=%+v want=%+v", got, value)
	}
}

func TestOptionalTimestampsAreAbsentWhenUnknown(t *testing.T) {
	value := struct {
		Scan   Scan        `json:"scan"`
		Trim   TrimStats   `json:"trim"`
		Bay    Bay         `json:"bay"`
		Health DriveHealth `json:"health"`
	}{}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ended_at", "action_time", "verified_at", "fresh_at", "0001-01-01"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("unknown timestamp %q leaked into JSON: %s", field, encoded)
		}
	}
}

func TestOptionalTimestampsMarshalAndRoundTripWhenKnown(t *testing.T) {
	want := time.Date(2026, 9, 3, 12, 34, 56, 789, time.UTC)
	value := struct {
		Scan   Scan        `json:"scan"`
		Trim   TrimStats   `json:"trim"`
		Bay    Bay         `json:"bay"`
		Health DriveHealth `json:"health"`
	}{
		Scan: Scan{EndedAt: want}, Trim: TrimStats{ActionTime: want},
		Bay: Bay{VerifiedAt: want}, Health: DriveHealth{FreshAt: want},
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ended_at", "action_time", "verified_at", "fresh_at"} {
		if !strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("known timestamp %q absent from JSON: %s", field, encoded)
		}
	}
	var got struct {
		Scan   Scan        `json:"scan"`
		Trim   TrimStats   `json:"trim"`
		Bay    Bay         `json:"bay"`
		Health DriveHealth `json:"health"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Scan.EndedAt.Equal(want) || !got.Trim.ActionTime.Equal(want) ||
		!got.Bay.VerifiedAt.Equal(want) || !got.Health.FreshAt.Equal(want) {
		t.Fatalf("timestamp round trip changed: %+v", got)
	}
}
