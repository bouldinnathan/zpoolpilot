// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
)

func TestLocateRefusesInferredBayMapping(t *testing.T) {
	bay := domain.Bay{
		Enclosure:  "/dev/sg2",
		Label:      "FRONT-03",
		Slot:       2,
		Source:     "manual",
		Confidence: inventory.BayConfidenceInferred,
		VerifiedAt: time.Now().UTC(),
	}
	err := validateLocateBay(bay)
	if err == nil || !strings.Contains(err.Error(), "manually verified") {
		t.Fatalf("validateLocateBay() error = %v, want inferred-mapping rejection", err)
	}
}

func TestSESDevicePathRejectsNonDeviceShapes(t *testing.T) {
	for _, path := range []string{"", "/dev/../tmp/sg2", "/dev//sg2", "/dev/sg", "/dev/sgx", "/dev/sda", "/dev/bsg/1:2:3", "/dev/bsg/1:2:3:4/extra"} {
		if inventory.ValidSESDevicePath(path) {
			t.Errorf("ValidSESDevicePath(%q) = true", path)
		}
	}
	for _, path := range []string{"/dev/sg0", "/dev/sg12", "/dev/bsg/1:2:3:4"} {
		if !inventory.ValidSESDevicePath(path) {
			t.Errorf("ValidSESDevicePath(%q) = false", path)
		}
	}
}

func TestSlotFlagDistinguishesOmittedFromExplicitZero(t *testing.T) {
	makeFlags := func(args ...string) *flag.FlagSet {
		t.Helper()
		flags := flag.NewFlagSet("slot-test", flag.ContinueOnError)
		flags.Int("slot", 0, "")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		return flags
	}
	if flagWasProvided(makeFlags(), "slot") {
		t.Fatal("omitted --slot was treated as authoritative slot zero")
	}
	if !flagWasProvided(makeFlags("--slot", "0"), "slot") {
		t.Fatal("explicit --slot 0 was not retained as authoritative")
	}
}

func TestLocateRequiresRecentPlausibleVerificationAndKnownSlot(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	valid := domain.Bay{
		Enclosure: "/dev/sg2", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "FRONT-03", Slot: 0, SlotKnown: true, Source: "manual",
		Confidence: inventory.BayConfidenceVerified, VerifiedAt: now.Add(-time.Minute),
	}
	if err := validateLocateBayAt(valid, now); err != nil {
		t.Fatalf("recent explicit slot zero rejected: %v", err)
	}
	tests := []struct {
		name string
		bay  domain.Bay
		text string
	}{
		{"stale", func() domain.Bay { bay := valid; bay.VerifiedAt = now.Add(-15*time.Minute - time.Second); return bay }(), "last 15 minutes"},
		{"future", func() domain.Bay { bay := valid; bay.VerifiedAt = now.Add(time.Minute + time.Second); return bay }(), "future"},
		{"unknown slot", func() domain.Bay { bay := valid; bay.SlotKnown = false; return bay }(), "known numeric slot"},
		{"volatile path", func() domain.Bay { bay := valid; bay.Enclosure = "/dev/../tmp/sg2"; return bay }(), "direct /dev"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLocateBayAt(test.bay, now); err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("validateLocateBayAt() error = %v, want %q", err, test.text)
			}
		})
	}
}

func TestLocateOffAllowsAbsentDriveAndStaleVerificationOnlyForClear(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	stale := domain.Bay{
		Enclosure: "/dev/sg2", EnclosureIdentity: "wwn:5000c500deadbeef",
		Label: "FRONT-03", Slot: 2, SlotKnown: true, Source: "manual",
		Confidence: inventory.BayConfidenceVerified, VerifiedAt: now.Add(-24 * time.Hour),
	}
	// This mapping deliberately has no corresponding drive snapshot. Clearing
	// an LED uses the exact authoritative key and structural enclosure state.
	mappings := map[string]domain.Bay{"wwn:5000c500aabbcc08": stale}
	bay, err := exactStoredBayMapping(mappings, "wwn:5000c500aabbcc08")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLocateBayForStateAt(bay, "off", now); err != nil {
		t.Fatalf("stale absent-drive mapping could not clear identify LED: %v", err)
	}
	if err := validateLocateBayForStateAt(bay, "on", now); err == nil || !strings.Contains(err.Error(), "last 15 minutes") {
		t.Fatalf("stale mapping was allowed to turn identify LED on: %v", err)
	}

	inferred := stale
	inferred.Confidence = inventory.BayConfidenceInferred
	inferred.VerifiedAt = time.Time{}
	if err := validateLocateBayForStateAt(inferred, "off", now); err == nil || !strings.Contains(err.Error(), "manually verified") {
		t.Fatalf("inferred mapping was allowed to clear identify LED: %v", err)
	}
	invalidPath := stale
	invalidPath.Enclosure = "/dev/sda"
	if err := validateLocateBayForStateAt(invalidPath, "off", now); err == nil || !strings.Contains(err.Error(), "direct /dev") {
		t.Fatalf("non-SES mapping was allowed to clear identify LED: %v", err)
	}
	future := stale
	future.VerifiedAt = now.Add(time.Minute + time.Second)
	if err := validateLocateBayForStateAt(future, "off", now); err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("implausibly future mapping was allowed to clear identify LED: %v", err)
	}
	for _, enclosureIdentity := range []string{"wwn:0000000000000000", "wwn:5000C500DEADBEEF", "scsi-serial:UNKNOWN123"} {
		invalidIdentity := stale
		invalidIdentity.EnclosureIdentity = enclosureIdentity
		if err := validateLocateBayForStateAt(invalidIdentity, "off", now); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Errorf("placeholder/noncanonical enclosure identity %q error = %v", enclosureIdentity, err)
		}
	}
	changed := stale
	changed.Slot = 3
	if sameAuthoritativeBayMapping(changed, stale) {
		t.Fatal("post-confirmation slot change would not refuse locate")
	}
}

func TestBayMutationAuthorityRequiresRootAndTTY(t *testing.T) {
	if err := requireBayMutationAuthority(0, true); err != nil {
		t.Fatalf("root on TTY rejected: %v", err)
	}
	if err := requireBayMutationAuthority(1000, true); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("non-root error = %v", err)
	}
	if err := requireBayMutationAuthority(0, false); err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("non-TTY error = %v", err)
	}
}

func TestLocateDriveIdentityRevalidationIncludesMajorMinor(t *testing.T) {
	original := domain.Drive{
		ID: "wwn:drive-a", WWN: "5000c50000112233", Serial: "serial-a",
		ByID: "/dev/disk/by-id/wwn-0x5000c50000112233", MajorMinor: "8:16",
		CapacityBytes: 1 << 40, LogicalSectorBytes: 512, PhysicalSectorBytes: 4096,
	}
	if !samePhysicalDriveIdentity(original, original) {
		t.Fatal("unchanged drive identity was rejected")
	}
	changedDevice := original
	changedDevice.MajorMinor = "8:32"
	if samePhysicalDriveIdentity(original, changedDevice) {
		t.Fatal("changed major:minor identity was accepted")
	}
	changedSerial := original
	changedSerial.Serial = "serial-b"
	if samePhysicalDriveIdentity(original, changedSerial) {
		t.Fatal("changed hardware identity was accepted")
	}
	missingAuthority := original
	missingAuthority.MajorMinor = ""
	if samePhysicalDriveIdentity(missingAuthority, missingAuthority) {
		t.Fatal("missing major:minor authority was accepted")
	}
}

func TestUnmapRequiresExactStoredKeyAndMappingsAreSorted(t *testing.T) {
	mappings := map[string]domain.Bay{
		"wwn:drive-b": {Label: "B2", Source: "manual", Confidence: inventory.BayConfidenceInferred},
		"wwn:drive-a": {Label: "A1", Source: "manual", Confidence: inventory.BayConfidenceInferred},
	}
	rows := sortedAuthoritativeBayMappings(mappings)
	if len(rows) != 2 || rows[0].DriveID != "wwn:drive-a" || rows[1].DriveID != "wwn:drive-b" {
		t.Fatalf("authoritative mappings are not deterministically sorted: %+v", rows)
	}
	if bay, err := exactStoredBayMapping(mappings, "wwn:drive-a"); err != nil || bay.Label != "A1" {
		t.Fatalf("exact stored key did not resolve: bay=%+v err=%v", bay, err)
	}
	if _, err := exactStoredBayMapping(mappings, "/dev/sda"); err == nil || !strings.Contains(err.Error(), "exact stored drive ID") {
		t.Fatalf("volatile alias resolved during unmap: %v", err)
	}
}

func TestUnmapIsClassifiedAsMutation(t *testing.T) {
	if !isMutationCommand([]string{"inventory", "unmap", "--drive", "wwn:retired"}) {
		t.Fatal("inventory unmap bypassed the global read-only mutation gate")
	}
	if isMutationCommand([]string{"inventory", "mappings", "--json"}) {
		t.Fatal("inventory mappings was incorrectly classified as a mutation")
	}
}
