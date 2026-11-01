// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import "testing"

func TestCanonicalSESIdentities(t *testing.T) {
	wwn, ok := CanonicalSESWWN("  0X5000C500DEADBEEF  ")
	if !ok || wwn != "wwn:5000c500deadbeef" || !ValidSESEnclosureIdentity(wwn) {
		t.Fatalf("canonical WWN = %q, ok=%t", wwn, ok)
	}
	serial, ok := CanonicalSESSerial("  ENC-AbC_0099  ")
	if !ok || serial != "scsi-serial:ENC-AbC_0099" || !ValidSESEnclosureIdentity(serial) {
		t.Fatalf("canonical serial = %q, ok=%t", serial, ok)
	}
	for _, identity := range []string{
		"wwn:0X5000C500DEADBEEF",
		"wwn:5000C500DEADBEEF",
		"scsi-serial: ENC-AbC_0099 ",
		"serial:ENC-AbC_0099",
	} {
		if ValidSESEnclosureIdentity(identity) {
			t.Errorf("noncanonical identity %q was accepted", identity)
		}
	}
}

func TestCanonicalDriveWWNForms(t *testing.T) {
	for input, want := range map[string]string{
		"0X5000C500DEADBEEF":   "5000c500deadbeef",
		"EUI.0011223344556677": "eui.0011223344556677",
		"naa.6001405abcde0001": "naa.6001405abcde0001",
	} {
		got, ok := CanonicalDriveWWN(input)
		if !ok || got != want {
			t.Errorf("CanonicalDriveWWN(%q) = %q, %t; want %q, true", input, got, ok, want)
		}
	}
	for _, input := range []string{"eui.0000000000000000", "naa.ffffffffffffffff", "eui.not-hex"} {
		if got, ok := CanonicalDriveWWN(input); ok {
			t.Errorf("placeholder drive WWN %q accepted as %q", input, got)
		}
	}
}

func TestCanonicalSESSerialRejectsPlaceholders(t *testing.T) {
	for _, value := range []string{
		"", "generic", "123456", "0123456789", "9876543210", "0000-0000",
		"UNKNOWN123", "serial0001", "To_be_filled_by_O.E.M.", "not-available",
		"AAAAAA", "bad serial", "bad\nserial",
	} {
		if identity, ok := CanonicalSESSerial(value); ok {
			t.Errorf("placeholder serial %q accepted as %q", value, identity)
		}
	}
}

func TestCanonicalSESWWNRejectsPlaceholders(t *testing.T) {
	for _, value := range []string{
		"", "0000000000000000", "ffffffffffffffff", "0x0000000000000000",
		"5000c500nothex00", "5000c500deadbee",
	} {
		if identity, ok := CanonicalSESWWN(value); ok {
			t.Errorf("placeholder WWN %q accepted as %q", value, identity)
		}
	}
}
