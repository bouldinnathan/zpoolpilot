// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func newTestBayFileStore(t *testing.T) *BayFileStore {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &BayFileStore{
		path: filepath.Join(root, "bays.json"), trustRoot: root,
		expectedOwner: uint32(os.Geteuid()), requireRootWriter: false,
	}
}

func TestBayFileStoreRoundTripAndPermissions(t *testing.T) {
	store := newTestBayFileStore(t)
	want := domain.Bay{
		Enclosure: "/dev/sg2", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "FRONT-03", Slot: 0, SlotKnown: true, Source: "manual",
		Confidence: BayConfidenceVerified, VerifiedAt: time.Now().UTC(),
	}
	if err := store.SaveBay(context.Background(), "wwn:5000c50000000001", want); err != nil {
		t.Fatal(err)
	}
	mappings, err := store.LoadBays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := mappings["wwn:5000c50000000001"]
	if got.EnclosureIdentity != want.EnclosureIdentity || !got.SlotKnown || got.Slot != 0 || got.Confidence != BayConfidenceVerified {
		t.Fatalf("mapping = %+v, want %+v", got, want)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("bay store permissions = %04o, want 0644", info.Mode().Perm())
	}
}

func TestBayFileStoreDistinguishesMissingFileFromMissingParent(t *testing.T) {
	store := newTestBayFileStore(t)
	if _, err := store.LoadBays(context.Background()); !errors.Is(err, ErrBayStoreMissing) {
		t.Fatalf("missing final store error = %v, want ErrBayStoreMissing", err)
	}

	store.path = filepath.Join(filepath.Dir(store.path), "missing-parent", "bays.json")
	if _, err := store.LoadBays(context.Background()); err == nil || errors.Is(err, ErrBayStoreMissing) {
		t.Fatalf("missing parent error = %v, want visible path trust/configuration failure", err)
	}
}

func TestBayFileStoreRejectsPlaceholderHardwareIdentities(t *testing.T) {
	store := newTestBayFileStore(t)
	inferred := domain.Bay{Label: "FRONT-01", Source: "manual", Confidence: BayConfidenceInferred}
	for _, driveID := range []string{
		"wwn:0000000000000000",
		"wwn:5000C500DEADBEEF",
		"serial:123456|disk|1000",
		"serial:validserial|disk|0",
		"serial:validserial|disk|not-a-size",
	} {
		if err := store.SaveBay(context.Background(), driveID, inferred); err == nil {
			t.Errorf("placeholder/noncanonical drive identity %q was accepted", driveID)
		}
	}

	verified := domain.Bay{
		Enclosure: "/dev/sg2", EnclosureIdentity: "scsi-serial:UNKNOWN123",
		Label: "FRONT-01", Slot: 1, SlotKnown: true, Source: "manual",
		Confidence: BayConfidenceVerified, VerifiedAt: time.Now().UTC(),
	}
	if err := store.SaveBay(context.Background(), "wwn:5000c50000000001", verified); err == nil {
		t.Fatal("placeholder verified enclosure identity was accepted")
	}
}

func TestBayFileStoreDeleteAllowsReplacementToReuseAbsentDriveBay(t *testing.T) {
	store := newTestBayFileStore(t)
	oldBay := domain.Bay{
		Enclosure: "/dev/sg2", Label: "FRONT-03", Slot: 2, SlotKnown: true,
		Source: "manual", Confidence: BayConfidenceInferred,
	}
	if err := store.SaveBay(context.Background(), "wwn:5000c50000000003", oldBay); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBay(context.Background(), "wwn:5000c50000000003", oldBay); err != nil {
		t.Fatal(err)
	}
	mappings, err := store.LoadBays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, present := mappings["wwn:5000c50000000003"]; present {
		t.Fatal("retired absent-drive mapping remained after deletion")
	}
	if err := store.SaveBay(context.Background(), "wwn:5000c50000000004", oldBay); err != nil {
		t.Fatalf("replacement drive could not reuse the retired drive's bay: %v", err)
	}
}

func TestBayFileStoreDeleteComparesExpectedMappingUnderLock(t *testing.T) {
	store := newTestBayFileStore(t)
	stored := domain.Bay{Label: "FRONT-03", Source: "manual", Confidence: BayConfidenceInferred}
	if err := store.SaveBay(context.Background(), "serial:retired|disk|1000", stored); err != nil {
		t.Fatal(err)
	}
	expected := stored
	expected.Label = "FRONT-04"
	err := store.DeleteBay(context.Background(), "serial:retired|disk|1000", expected)
	if err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("changed mapping deletion error = %v", err)
	}
	mappings, loadErr := store.LoadBays(context.Background())
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, present := mappings["serial:retired|disk|1000"]; !present {
		t.Fatal("compare-and-delete removed a changed mapping")
	}
}

func TestBayFileStoreDeleteRejectsVolatileDriveKey(t *testing.T) {
	store := newTestBayFileStore(t)
	err := store.DeleteBay(context.Background(), "/dev/sda", domain.Bay{
		Label: "FRONT", Source: "manual", Confidence: BayConfidenceInferred,
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe drive identity") {
		t.Fatalf("volatile mapping deletion key accepted: %v", err)
	}
}

func TestBayFileStoreRejectsSymlinkAndWritableFile(t *testing.T) {
	store := newTestBayFileStore(t)
	valid := `{"api_version":"github.com/bouldinnathan/zpoolpilot/bays/v1","mappings":{}}`
	target := filepath.Join(filepath.Dir(store.path), "target.json")
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadBays(context.Background()); err == nil {
		t.Fatal("bay-store symlink was accepted")
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, []byte(valid), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadBays(context.Background()); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("group/other-writable bay store accepted: %v", err)
	}
}

func TestBayFileStoreRejectsDuplicateKnownSlotAcrossLabels(t *testing.T) {
	store := newTestBayFileStore(t)
	now := time.Now().UTC()
	first := domain.Bay{
		Enclosure: "/dev/sg2", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "LEFT", Slot: 4, SlotKnown: true, Source: "manual",
		Confidence: BayConfidenceVerified, VerifiedAt: now,
	}
	if err := store.SaveBay(context.Background(), "wwn:5000c50000000001", first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Label = "RIGHT"
	if err := store.SaveBay(context.Background(), "wwn:5000c50000000002", second); err == nil {
		t.Fatal("duplicate stable enclosure slot was accepted")
	}
}

func TestBayFileStoreLoadRejectsDuplicateKnownSlotAcrossLabels(t *testing.T) {
	store := newTestBayFileStore(t)
	now := time.Now().UTC()
	first := domain.Bay{
		Enclosure: "/dev/sg2", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "LEFT", Slot: 4, SlotKnown: true, Source: "manual",
		Confidence: BayConfidenceVerified, VerifiedAt: now,
	}
	second := first
	second.Label = "RIGHT"
	data, err := json.Marshal(bayFile{APIVersion: BayStoreAPIVersion, Mappings: map[string]domain.Bay{
		"wwn:5000c50000000001": first,
		"wwn:5000c50000000002": second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadBays(context.Background()); err == nil || !strings.Contains(err.Error(), "same physical bay") {
		t.Fatalf("duplicate mapping file accepted: %v", err)
	}
}

func TestValidateStoredBayRejectsAmbiguousUnknownSlot(t *testing.T) {
	bay := domain.Bay{
		Label: "FRONT", Slot: 7, SlotKnown: false, Source: "manual",
		Confidence: BayConfidenceInferred,
	}
	if err := validateStoredBay(bay); err == nil || !strings.Contains(err.Error(), "must be zero") {
		t.Fatalf("ambiguous unknown slot accepted: %v", err)
	}
}

func TestBayFileStoreRejectsVolatileDriveKey(t *testing.T) {
	store := newTestBayFileStore(t)
	err := store.SaveBay(context.Background(), "path:/dev/sda", domain.Bay{
		Label: "FRONT", Source: "manual", Confidence: BayConfidenceInferred,
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe drive identity") {
		t.Fatalf("volatile mapping key accepted: %v", err)
	}
}

func TestBayFileStoreCanceledWritesPreserveMappings(t *testing.T) {
	store := newTestBayFileStore(t)
	bay := domain.Bay{Label: "FRONT", Source: "manual", Confidence: BayConfidenceInferred}
	if err := store.SaveBay(context.Background(), testDriveAID, bay); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	updated := bay
	updated.Label = "REAR"
	if err := store.SaveBay(ctx, testDriveAID, updated); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save returned %v", err)
	}
	if err := store.DeleteBay(ctx, testDriveAID, bay); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delete returned %v", err)
	}
	mappings, err := store.LoadBays(context.Background())
	if err != nil || !equalStoredBay(mappings[testDriveAID], bay) {
		t.Fatalf("canceled writes changed the mapping: %+v, %v", mappings, err)
	}
}
