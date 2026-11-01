// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import "testing"

func TestParseProxmoxBootMode(t *testing.T) {
	tests := []struct {
		output string
		want   string
		bad    bool
	}{
		{"ABCD is configured with: uefi (versions: 6.17)", "systemd-boot", false},
		{"ABCD is configured with: grub (versions: 6.17)", "grub", false},
		{"A is configured with: uefi\nB is configured with: grub", "", true},
		{"A is configured with: uefi\nB is configured with: legacy-custom", "", true},
		{"A is configured with: uefi and grub", "", true},
		{"System currently booted with uefi", "", true},
	}
	for _, test := range tests {
		got, err := ParseProxmoxBootMode(test.output)
		if got != test.want || (err != nil) != test.bad {
			t.Fatalf("mode=%q err=%v, want=%q bad=%t", got, err, test.want, test.bad)
		}
	}
}
