// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

const (
	testDriveAID  = "wwn:5000c50000000001"
	testDriveAWWN = "5000c50000000001"
	testDriveBID  = "wwn:5000c50000000002"
	testDriveBWWN = "5000c50000000002"
)

type fakeStore struct {
	bays      map[string]domain.Bay
	loadErr   error
	loadCalls int
}

func (f *fakeStore) LoadBays(_ context.Context) (map[string]domain.Bay, error) {
	f.loadCalls++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	result := make(map[string]domain.Bay, len(f.bays))
	for id, bay := range f.bays {
		result[id] = bay
	}
	return result, nil
}

func TestMergeMappedBaysUsesOneAuthenticatedSnapshotAndClearsRejectedCache(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{
		{ID: "a", Bay: domain.Bay{Label: "old-a", Source: "manual", Confidence: BayConfidenceVerified}},
		{ID: "b", Bay: domain.Bay{Label: "old-b", Source: "manual", Confidence: BayConfidenceVerified}},
	}}
	store := &fakeStore{loadErr: errors.New("untrusted mapping file")}
	err := MergeMappedBays(context.Background(), &s, store)
	if err == nil || store.loadCalls != 1 {
		t.Fatalf("merge error=%v load calls=%d, want one rejected bulk read", err, store.loadCalls)
	}
	for _, drive := range s.Drives {
		if drive.Bay.Source == "manual" || drive.Bay.Label != "" {
			t.Fatalf("cached manual mapping survived rejected authority: %+v", drive.Bay)
		}
	}
}
func (f *fakeStore) SaveBay(_ context.Context, id string, b domain.Bay) error {
	if f.bays == nil {
		f.bays = map[string]domain.Bay{}
	}
	f.bays[id] = b
	return nil
}

func TestMapBayStoresInferredManualMapping(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{
		{ID: testDriveAID, WWN: testDriveAWWN, Serial: "SERIAL-A"},
		{ID: testDriveBID, WWN: testDriveBWWN, Serial: "SERIAL-B"},
	}}
	f := &fakeStore{}
	if err := MapBay(context.Background(), f, s, "SERIAL-A", domain.Bay{Enclosure: "E", Label: "01", Slot: 1, SlotKnown: true, Confidence: BayConfidenceInferred}); err != nil {
		t.Fatal(err)
	}
	stored := f.bays[testDriveAID]
	if stored.Source != "manual" || stored.Confidence != BayConfidenceInferred || !stored.VerifiedAt.IsZero() {
		t.Fatalf("stored mapping = %+v, want unverified manual mapping", stored)
	}
	MergeMappedBays(context.Background(), &s, f)
	if s.Drives[0].Bay.Label != "01" || s.Drives[0].Bay.Confidence != BayConfidenceInferred {
		t.Fatalf("got %+v", s.Drives[0].Bay)
	}
}

func TestMapBayVerifyPromotesStoredMapping(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{{ID: testDriveAID, WWN: testDriveAWWN, Serial: "SERIAL-A"}}}
	f := &fakeStore{}
	bay := domain.Bay{Enclosure: "/dev/sg2", Label: "FRONT-03", Slot: 2, SlotKnown: true, Confidence: BayConfidenceInferred}
	if err := MapBay(context.Background(), f, s, "SERIAL-A", bay); err != nil {
		t.Fatal(err)
	}
	bay.Confidence = BayConfidenceVerified
	bay.EnclosureIdentity = "wwn:5000c500deadbeef"
	if err := MapBay(context.Background(), f, s, "SERIAL-A", bay); err != nil {
		t.Fatal(err)
	}
	stored := f.bays[testDriveAID]
	if stored.Source != "manual" || stored.Confidence != BayConfidenceVerified || stored.VerifiedAt.IsZero() {
		t.Fatalf("stored mapping = %+v, want verified manual mapping with timestamp", stored)
	}
}

func TestMapBayDistinguishesUnknownSlotFromExplicitZero(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{{ID: testDriveAID, WWN: testDriveAWWN, Serial: "SERIAL-A"}}}
	f := &fakeStore{}
	if err := MapBay(context.Background(), f, s, "SERIAL-A", domain.Bay{Label: "FRONT", Confidence: BayConfidenceInferred}); err != nil {
		t.Fatal(err)
	}
	if stored := f.bays[testDriveAID]; stored.SlotKnown || stored.Slot != 0 {
		t.Fatalf("omitted slot stored as %+v, want unknown", stored)
	}
	verified := domain.Bay{
		Enclosure: "/dev/sg0", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "FRONT", Slot: 0, SlotKnown: true, Confidence: BayConfidenceVerified,
	}
	if err := MapBay(context.Background(), f, s, "SERIAL-A", verified); err != nil {
		t.Fatal(err)
	}
	stored := f.bays[testDriveAID]
	if !stored.SlotKnown || stored.Slot != 0 {
		t.Fatalf("explicit slot zero stored as %+v", stored)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"slot":0`) || !strings.Contains(string(encoded), `"slot_known":true`) {
		t.Fatalf("explicit slot zero is ambiguous on wire: %s", encoded)
	}
}

func TestMapBayVerifyRejectsUnknownSlot(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{{ID: testDriveAID, WWN: testDriveAWWN, Serial: "SERIAL-A"}}}
	err := MapBay(context.Background(), &fakeStore{}, s, "SERIAL-A", domain.Bay{
		Enclosure: "/dev/sg0", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "FRONT", Confidence: BayConfidenceVerified,
	})
	if err == nil || !strings.Contains(err.Error(), "explicitly known") {
		t.Fatalf("MapBay() error = %v, want unknown-slot rejection", err)
	}
}

func TestMapBayRequiresExplicitConfidence(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{{ID: testDriveAID, WWN: testDriveAWWN, Serial: "SERIAL-A"}}}
	err := MapBay(context.Background(), &fakeStore{}, s, "SERIAL-A", domain.Bay{Label: "01"})
	if err == nil {
		t.Fatal("MapBay accepted a mapping without explicit confidence")
	}
}

func TestMapBayRejectsVolatileAndAmbiguousHardwareIdentity(t *testing.T) {
	pathOnly := domain.Snapshot{Drives: []domain.Drive{{ID: "path:/dev/sda", DevNode: "/dev/sda"}}}
	if err := MapBay(context.Background(), &fakeStore{}, pathOnly, "/dev/sda", domain.Bay{Label: "A", Confidence: BayConfidenceInferred}); err == nil || !strings.Contains(err.Error(), "volatile path") {
		t.Fatalf("path-only identity accepted: %v", err)
	}

	duplicateSerial := domain.Snapshot{Drives: []domain.Drive{
		{ID: "serial:same-serial|model-a|1000", DevNode: "/dev/sda", Serial: "SAME-SERIAL", Model: "MODEL-A", CapacityBytes: 1000},
		{ID: "serial:same-serial|model-b|1000", DevNode: "/dev/sdb", Serial: "same-serial", Model: "MODEL-B", CapacityBytes: 1000},
	}}
	if err := MapBay(context.Background(), &fakeStore{}, duplicateSerial, "/dev/sda", domain.Bay{Label: "A", Confidence: BayConfidenceInferred}); err == nil || !strings.Contains(err.Error(), "serial resolves to 2") {
		t.Fatalf("duplicate serial accepted: %v", err)
	}

	duplicateWWN := domain.Snapshot{Drives: []domain.Drive{
		{ID: "wwn:5000c500deadbeef", DevNode: "/dev/sda", WWN: "5000C500DEADBEEF", Serial: "SERIAL-A"},
		{ID: "wwn:5000c500deadbeef", DevNode: "/dev/sdb", WWN: "5000c500deadbeef", Serial: "SERIAL-B"},
	}}
	if err := MapBay(context.Background(), &fakeStore{}, duplicateWWN, "/dev/sda", domain.Bay{Label: "A", Confidence: BayConfidenceInferred}); err == nil || !strings.Contains(err.Error(), "stable identity resolves to 2") {
		t.Fatalf("duplicate WWN accepted: %v", err)
	}

	placeholderWWN := domain.Snapshot{Drives: []domain.Drive{{
		ID: "wwn:0000000000000000", DevNode: "/dev/sda", WWN: "0000000000000000", Serial: "VALID-SERIAL",
	}}}
	if err := MapBay(context.Background(), &fakeStore{}, placeholderWWN, "/dev/sda", domain.Bay{Label: "A", Confidence: BayConfidenceInferred}); err == nil || !strings.Contains(err.Error(), "placeholder WWN") {
		t.Fatalf("placeholder WWN accepted: %v", err)
	}

	placeholderSerial := domain.Snapshot{Drives: []domain.Drive{{
		ID: "serial:123456|model|1000", DevNode: "/dev/sda", Serial: "123456", Model: "MODEL", CapacityBytes: 1000,
	}}}
	if err := MapBay(context.Background(), &fakeStore{}, placeholderSerial, "/dev/sda", domain.Bay{Label: "A", Confidence: BayConfidenceInferred}); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("placeholder serial accepted: %v", err)
	}
}

func TestConflict(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{{ID: "a", Bay: domain.Bay{Label: "1"}}, {ID: "b", Bay: domain.Bay{Label: "1"}}}}
	DetectConflicts(&s)
	DetectConflicts(&s)
	if len(s.Findings) != 1 || s.Drives[0].Bay.Confidence != "conflict" {
		t.Fatalf("got %+v", s)
	}
}

func TestConflictDetectsSameKnownSlotWithDifferentLabels(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{
		{ID: "a", Bay: domain.Bay{EnclosureIdentity: "wwn:5000c500deadbeef", Label: "LEFT", Slot: 0, SlotKnown: true}},
		{ID: "b", Bay: domain.Bay{EnclosureIdentity: "wwn:5000c500deadbeef", Label: "RIGHT", Slot: 0, SlotKnown: true}},
	}}
	DetectConflicts(&s)
	if len(s.Findings) != 1 || s.Drives[0].Bay.Confidence != "conflict" || s.Drives[1].Bay.Confidence != "conflict" {
		t.Fatalf("same enclosure slot was not detected: %+v", s)
	}
}

func TestDetectConflictsClearsResolvedCachedConflictState(t *testing.T) {
	s := domain.Snapshot{
		Drives: []domain.Drive{
			{ID: "a", Bay: domain.Bay{Enclosure: "E", Label: "1", Confidence: "conflict"}},
			{ID: "b", Bay: domain.Bay{Enclosure: "E", Label: "2", Confidence: "CONFLICT"}},
		},
		Findings: []domain.Finding{
			{ID: "bay-conflict-a-b", Title: "stale conflict"},
			{ID: "unrelated", Title: "keep me"},
		},
	}

	DetectConflicts(&s)

	if got := s.Drives[0].Bay.Confidence; got != "" {
		t.Fatalf("drive a confidence = %q, want stale conflict cleared", got)
	}
	if got := s.Drives[1].Bay.Confidence; got != "" {
		t.Fatalf("drive b confidence = %q, want stale conflict cleared", got)
	}
	if len(s.Findings) != 1 || s.Findings[0].ID != "unrelated" {
		t.Fatalf("findings = %+v, want only unrelated finding", s.Findings)
	}
}

func TestDetectConflictsRestoresResolvedByPathInference(t *testing.T) {
	s := domain.Snapshot{Drives: []domain.Drive{
		{ID: "a", Bay: domain.Bay{Enclosure: "linux-by-path", Label: "path-a", Source: "by-path", Confidence: "conflict"}},
		{ID: "b", Bay: domain.Bay{Enclosure: "linux-by-path", Label: "path-b", Source: "by-path", Confidence: "conflict"}},
	}}

	DetectConflicts(&s)

	for _, drive := range s.Drives {
		if drive.Bay.Confidence != BayConfidenceInferred {
			t.Fatalf("resolved by-path confidence = %q, want inferred", drive.Bay.Confidence)
		}
	}
}

func TestMergeMappedBaysClearsStaleConflictBeforeTrustedMappings(t *testing.T) {
	s := domain.Snapshot{
		Drives: []domain.Drive{
			{ID: "a", Bay: domain.Bay{Enclosure: "old", Label: "1", Confidence: "conflict"}},
			{ID: "b", Bay: domain.Bay{Enclosure: "old", Label: "1", Confidence: "conflict"}},
		},
		Findings: []domain.Finding{
			{ID: "bay-conflict-a-b", Title: "stale conflict"},
			{ID: "unrelated", Title: "keep me"},
		},
	}
	store := &fakeStore{bays: map[string]domain.Bay{
		"a": {Enclosure: "E", Label: "1", Source: "manual", Confidence: BayConfidenceVerified},
		"b": {Enclosure: "E", Label: "2", Source: "manual", Confidence: BayConfidenceInferred},
	}}

	MergeMappedBays(context.Background(), &s, store)

	if got := s.Drives[0].Bay; got.Label != "1" || got.Confidence != BayConfidenceVerified {
		t.Fatalf("drive a bay = %+v, want current verified mapping", got)
	}
	if got := s.Drives[1].Bay; got.Label != "2" || got.Confidence != BayConfidenceInferred {
		t.Fatalf("drive b bay = %+v, want current inferred mapping", got)
	}
	if len(s.Findings) != 1 || s.Findings[0].ID != "unrelated" {
		t.Fatalf("findings = %+v, want stale bay conflict removed", s.Findings)
	}
}

func TestMergeMappedBaysRegeneratesCurrentConflict(t *testing.T) {
	s := domain.Snapshot{
		Drives: []domain.Drive{
			{ID: "a", Bay: domain.Bay{Enclosure: "old", Label: "1", Confidence: "conflict"}},
			{ID: "b", Bay: domain.Bay{Enclosure: "old", Label: "1", Confidence: "conflict"}},
		},
		Findings: []domain.Finding{{ID: "bay-conflict-a-b", Title: "stale conflict", Evidence: "old evidence"}},
	}
	store := &fakeStore{bays: map[string]domain.Bay{
		"a": {Enclosure: "new", Label: "9", Source: "manual", Confidence: BayConfidenceVerified},
		"b": {Enclosure: "new", Label: "9", Source: "manual", Confidence: BayConfidenceVerified},
	}}

	MergeMappedBays(context.Background(), &s, store)

	if len(s.Findings) != 1 || s.Findings[0].ID != "bay-conflict-a-b" {
		t.Fatalf("findings = %+v, want one recomputed bay conflict", s.Findings)
	}
	if s.Findings[0].Title == "stale conflict" || s.Findings[0].Evidence == "old evidence" {
		t.Fatalf("conflict finding was retained instead of recomputed: %+v", s.Findings[0])
	}
	if s.Drives[0].Bay.Confidence != "conflict" || s.Drives[1].Bay.Confidence != "conflict" {
		t.Fatalf("drive confidences = %q, %q; want current conflict", s.Drives[0].Bay.Confidence, s.Drives[1].Bay.Confidence)
	}
}

func TestLookupsRejectEmptyQueries(t *testing.T) {
	snapshot := domain.Snapshot{
		Drives: []domain.Drive{{ID: testDriveAID}},
		Pools:  []domain.Pool{{Name: "tank", VDevs: []domain.VDev{{Name: "mirror-0"}}}},
	}
	for _, query := range []string{"", " \t\n"} {
		if _, ok := FindDrive(snapshot, query); ok {
			t.Errorf("empty drive query %q selected a disk", query)
		}
		if _, ok := FindPool(snapshot, query); ok {
			t.Errorf("empty pool query %q selected a pool", query)
		}
		if _, ok := FindVDev(snapshot.Pools[0], query); ok {
			t.Errorf("empty vdev query %q selected a vdev", query)
		}
	}
}

func TestFindPoolRejectsAmbiguousIdentity(t *testing.T) {
	snapshot := domain.Snapshot{Pools: []domain.Pool{{Name: "tank", GUID: "42"}, {Name: "tank", GUID: "43"}}}
	if _, ok := FindPool(snapshot, "tank"); ok {
		t.Fatal("ambiguous pool name selected the first match")
	}
	if got, ok := FindPool(snapshot, "42"); !ok || got.GUID != "42" {
		t.Fatalf("unique pool GUID lookup = %+v, %v", got, ok)
	}
	snapshot.Pools[1].Name = "42"
	if _, ok := FindPool(snapshot, "42"); ok {
		t.Fatal("query shared by a GUID and another pool name was accepted")
	}
}
