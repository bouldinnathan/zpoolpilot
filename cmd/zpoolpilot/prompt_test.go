// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

func TestDisplayCommandQuotesWithoutEvaluation(t *testing.T) {
	got := displayCommand("zpool", []string{"create", "bad; touch /tmp/no", "a'b", "$HOME"})
	for _, want := range []string{"'bad; touch /tmp/no'", `'a'\''b'`, `'$HOME'`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestTerminalTextRemovesControlAndDirectionSpoofing(t *testing.T) {
	input := "safe\x1b[2J\rforged\a\u202eyes"
	got := terminalText(input)
	for _, forbidden := range []string{"\x1b", "\r", "\a", "\u202e"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("terminal payload %q survived in %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "safe") || !strings.Contains(got, "forged") {
		t.Fatalf("printable context was lost: %q", got)
	}
	if got := displayCommand("zpool\x1b[2J", []string{"status\rFORGED"}); strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("command preview contains terminal controls: %q", got)
	}
}
