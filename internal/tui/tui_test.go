// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestSummaryNoColor(t *testing.T) {
	supported := true
	s := domain.Snapshot{Generated: time.Now(), Pools: []domain.Pool{{Name: "rpool", State: "DEGRADED", BootCritical: true, CapacityPercent: 81, FreeBytes: 1024, Trim: domain.TrimStats{State: "active", Percent: 25, Errors: 1, Supported: &supported}}}}
	var b bytes.Buffer
	Summary(&b, s, false)
	out := b.String()
	for _, want := range []string{"[RED]", "rpool", "BOOT-CRITICAL", "trim=active 25.0%", "support=yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("unexpected ANSI")
	}
}

func TestQuoteArgs(t *testing.T) {
	got := quoteArgs([]string{"get", "tank/with space", "a'b"})
	if !strings.Contains(got, "'tank/with space'") || !strings.Contains(got, `'a'\''b'`) {
		t.Fatalf("got %s", got)
	}
}

func TestRunLineModeNavigationAndHelp(t *testing.T) {
	input := strings.NewReader("2\n?\nh\n8\nq\n")
	var output bytes.Buffer
	err := Run(context.Background(), richSnapshot(), Options{In: input, Out: &output, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"press Enter after a key",
		"Pools / VDevs",
		"KEYBOARD",
		"Line mode",
		"Command Trace",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\x1b[?1049h") {
		t.Fatal("line-mode output unexpectedly used full-screen control sequences")
	}
}

func TestRunRefreshDoesNotBlockInputLoop(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	var output bytes.Buffer
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), domain.Snapshot{}, Options{
			In:  reader,
			Out: &output,
			Refresh: func(context.Context) (domain.Snapshot, error) {
				close(refreshStarted)
				<-releaseRefresh
				return domain.Snapshot{Generated: time.Now(), Host: domain.Host{Hostname: "refreshed"}}, nil
			},
		})
	}()
	if _, err := io.WriteString(writer, "r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	// Quit must be handled even though the collector callback is still waiting.
	if _, err := io.WriteString(writer, "q\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run remained blocked behind refresh")
	}
	close(releaseRefresh)
	_ = writer.Close()
}

func TestRawInputSequences(t *testing.T) {
	remaining, keys := parseRawBytes([]byte("n\x1b[C\x1b[D\x1b[A\x1b[B\x1b[1~\x1b[4~\t\r\x04"))
	if len(remaining) != 0 {
		t.Fatalf("unexpected remaining bytes %q", remaining)
	}
	want := []string{"n", "right", "left", "up", "down", "home", "end", "tab", "next", "ctrl-d"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys %#v, want %#v", keys, want)
	}
}

func TestRawInputPreservesSplitSequence(t *testing.T) {
	remaining, keys := parseRawBytes([]byte("\x1b["))
	if len(keys) != 0 || string(remaining) != "\x1b[" {
		t.Fatalf("remaining %q, keys %#v", remaining, keys)
	}
	remaining = append(remaining, 'C', 'q')
	remaining, keys = parseRawBytes(remaining)
	if len(remaining) != 0 || strings.Join(keys, ",") != "right,q" {
		t.Fatalf("remaining %q, keys %#v", remaining, keys)
	}
}

func TestRenderFrameHonorsTerminalBounds(t *testing.T) {
	const width, height = 48, 9
	frame := renderFrame(richSnapshot(), 2, "a deliberately long status that must fit", false, Options{Color: true}, width, height)
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	if len(lines) > height {
		t.Fatalf("rendered %d lines, terminal height %d", len(lines), height)
	}
	for i, line := range lines {
		if got := visibleWidth(line); got > width {
			t.Errorf("line %d is %d columns: %q", i, got, line)
		}
	}
	if !strings.Contains(frame, "more line") {
		t.Fatalf("expected overflow notice in:\n%s", frame)
	}
	if !strings.Contains(frame, "\x1b[") {
		t.Fatal("expected color escape sequence")
	}
}

func TestRenderFrameScrollsToHiddenContent(t *testing.T) {
	snapshot := richSnapshot()
	first := renderFrameAt(snapshot, 2, 0, "ok", false, Options{}, 100, 9)
	last := renderFrameAt(snapshot, 2, 99, "ok", false, Options{}, 100, 9)
	if !strings.Contains(first, "more line") {
		t.Fatalf("first viewport lacks scroll affordance:\n%s", first)
	}
	if !strings.Contains(last, "UNKNOWN BAY") {
		t.Fatalf("last viewport does not expose final content:\n%s", last)
	}
	if !strings.Contains(last, "view") {
		t.Fatalf("scrolled viewport lacks position:\n%s", last)
	}
}

func TestRenderRichPagesRemainTruthful(t *testing.T) {
	snapshot := richSnapshot()
	tests := []struct {
		page int
		want []string
	}{
		{0, []string{"node-a", "COLLECTION GAPS", "Evidence:", "Suggested:"}},
		{1, []string{"BOOT-CRITICAL", "data/mirror", "/dev/disk/by-id/ata-one", "trim: active", "supported=yes"}},
		{2, []string{"rack-a/Bay 01", "bay source=manual", "life used 12.5%", "sampled"}},
		{3, []string{"compression=lz4 (local)", "dedup=off (default)", "raw pool free space"}},
		{4, []string{"host-wide", "hit ratio 80.00%", "per-vdev I/O rates"}},
		{5, []string{"scrub", "50.00%", "manual trim", "25.00%", "point-in-time"}},
		{6, []string{"typed, expiring plans", "prepare-disk", "[RED]"}},
		{7, []string{"green-read", "capability=observe", "output hidden", "zpool status"}},
	}
	for _, tt := range tests {
		frame := renderFrame(snapshot, tt.page, "ok", false, Options{}, 220, 80)
		for _, want := range tt.want {
			if !strings.Contains(frame, want) {
				t.Errorf("page %d missing %q in:\n%s", tt.page+1, want, frame)
			}
		}
	}
}

func TestAdversarialSnapshotPlainOutputHasNoTerminalControls(t *testing.T) {
	payload := adversarialTerminalText()
	out := renderAdversarialOutput(t, false, payload)

	if strings.Contains(out, payload) {
		t.Fatal("raw attacker payload reached plain TUI output")
	}
	if !strings.Contains(out, "\uFFFD") {
		t.Fatal("sanitized output does not visibly mark rejected controls")
	}
	assertNoTerminalControls(t, out, false)
}

func TestAdversarialSnapshotColorOutputOnlyHasInternalANSI(t *testing.T) {
	payload := adversarialTerminalText()
	out := renderAdversarialOutput(t, true, payload)

	if strings.Contains(out, payload) {
		t.Fatal("raw attacker payload reached colored TUI output")
	}
	if !strings.Contains(out, "\uFFFD") {
		t.Fatal("sanitized output does not visibly mark rejected controls")
	}
	assertNoTerminalControls(t, out, true)
}

func TestUnknownHealthIsNotGreen(t *testing.T) {
	var output bytes.Buffer
	drives(&output, domain.Snapshot{Drives: []domain.Drive{{ID: "disk-a"}}}, false)
	if !strings.Contains(output.String(), "[UNKNOWN]") || strings.Contains(output.String(), "[GREEN]") {
		t.Fatalf("unexpected health label: %s", output.String())
	}
}

func TestTruncateANSIPreservesVisibleBound(t *testing.T) {
	input := paint(true, "[RED]", "1;31") + " abcdefghijklmnopqrstuvwxyz"
	got := truncateANSI(input, 12)
	if visibleWidth(got) > 12 {
		t.Fatalf("visible width %d: %q", visibleWidth(got), got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("truncated colored string has no reset: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("truncated string has no ellipsis: %q", got)
	}
}

func TestRunReportsReaderAndWriterErrors(t *testing.T) {
	err := Run(context.Background(), domain.Snapshot{}, Options{In: errorReader{}, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "read terminal input") {
		t.Fatalf("reader error = %v", err)
	}
	err = Run(context.Background(), domain.Snapshot{}, Options{In: strings.NewReader("q\n"), Out: errorWriter{}})
	if err == nil || !strings.Contains(err.Error(), "render dashboard") {
		t.Fatalf("writer error = %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func renderAdversarialOutput(t *testing.T, color bool, payload string) string {
	t.Helper()
	snapshot := richSnapshot()
	snapshot.APIVersion = payload
	snapshot.Host = domain.Host{
		Hostname:        payload,
		MachineID:       payload,
		OS:              payload,
		Kernel:          payload,
		OpenZFSVersion:  payload,
		ProxmoxVersion:  payload,
		ProxmoxNode:     payload,
		ProxmoxBootMode: payload,
	}
	pool := &snapshot.Pools[0]
	pool.Name = payload
	pool.GUID = payload
	pool.State = payload
	pool.Status = payload
	pool.Action = payload
	pool.Scan.Function = payload
	pool.Scan.State = payload
	pool.Trim.State = payload
	vdev := &pool.VDevs[0]
	vdev.Name = payload
	vdev.GUID = payload
	vdev.Type = payload
	vdev.Class = payload
	vdev.State = payload
	vdev.Path = payload
	vdev.StableDriveID = payload
	vdev.Trim.State = payload
	vdev = &vdev.Children[0]
	vdev.Name = payload
	vdev.Path = payload

	drive := &snapshot.Drives[0]
	drive.ID = payload
	drive.DevNode = payload
	drive.MajorMinor = payload
	drive.ByID = payload
	drive.SysfsPath = payload
	drive.WWN = payload
	drive.Serial = payload
	drive.Vendor = payload
	drive.Model = payload
	drive.Firmware = payload
	drive.Transport = payload
	drive.MediaType = payload
	drive.Use = payload
	drive.Bay.Enclosure = payload
	drive.Bay.Label = payload
	drive.Bay.Source = payload
	drive.Bay.Confidence = payload
	drive.Health.Severity = domain.Severity(payload)
	drive.Health.Message = payload

	dataset := &snapshot.Datasets[0]
	dataset.Name = payload
	dataset.GUID = payload
	dataset.Type = payload
	for key := range dataset.Properties {
		dataset.Properties[key] = payload
	}
	for key := range dataset.PropertySources {
		dataset.PropertySources[key] = payload
	}
	dataset.Properties[payload] = payload
	dataset.PropertySources[payload] = payload

	snapshot.Findings[0] = domain.Finding{
		ID:          payload,
		Severity:    domain.Severity(payload),
		Scope:       payload,
		Title:       payload,
		Evidence:    payload,
		Remediation: payload,
	}
	snapshot.Errors[0] = domain.SourceError{Source: payload, Kind: payload, Message: payload, Partial: true}
	snapshot.Commands[0].Program = payload
	snapshot.Commands[0].Args = []string{payload}
	snapshot.Commands[0].Capability = payload
	snapshot.Commands[0].Risk = domain.Risk(payload)
	snapshot.Commands[0].Error = payload

	var output strings.Builder
	Summary(&output, snapshot, color)
	status := "Refresh failed: " + payload
	for page := range pages {
		output.WriteString(renderFrame(snapshot, page, status, false, Options{Color: color}, 1000, 500))
	}
	return output.String()
}

func adversarialTerminalText() string {
	var value strings.Builder
	value.WriteString("ATTACK-BEGIN")
	for r := rune(0); r <= 0x1f; r++ {
		value.WriteRune(r)
	}
	for r := rune(0x7f); r <= 0x9f; r++ {
		value.WriteRune(r)
	}
	for _, r := range []rune{
		'\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069', '\u206a', '\u206f',
	} {
		value.WriteRune(r)
	}
	value.WriteString("\x1b[31mCSI-PAYLOAD\x1b[0m\x1b]2;OSC-PAYLOAD\x07ATTACK-END")
	return value.String()
}

func assertNoTerminalControls(t *testing.T, output string, allowInternalANSI bool) {
	t.Helper()
	allowedANSI := map[string]bool{
		"\x1b[0m":       true,
		"\x1b[1;31m":    true,
		"\x1b[1;32m":    true,
		"\x1b[1;33m":    true,
		"\x1b[1;35m":    true,
		"\x1b[1;36m":    true,
		"\x1b[1;37;44m": true,
	}
	sawANSI := false
	for offset := 0; offset < len(output); {
		if output[offset] == 0x1b {
			if !allowInternalANSI {
				t.Fatalf("plain output contains ESC at byte %d", offset)
			}
			end := ansiSequenceEnd(output[offset:])
			if end == 0 {
				t.Fatalf("colored output contains non-CSI ESC at byte %d", offset)
			}
			sequence := output[offset : offset+end]
			if !allowedANSI[sequence] {
				t.Fatalf("colored output contains unexpected CSI %q", sequence)
			}
			sawANSI = true
			offset += end
			continue
		}
		r, size := utf8.DecodeRuneInString(output[offset:])
		if r == utf8.RuneError && size == 1 {
			t.Fatalf("output contains invalid UTF-8 at byte %d", offset)
		}
		if r != '\n' && isUnsafeTerminalRune(r) {
			t.Fatalf("output contains terminal control %U at byte %d", r, offset)
		}
		offset += size
	}
	if allowInternalANSI && !sawANSI {
		t.Fatal("colored output did not contain expected internal ANSI styling")
	}
}

func richSnapshot() domain.Snapshot {
	now := time.Now().Add(-time.Minute)
	passed := true
	temp := 34.0
	used := 12.5
	hours := uint64(1234)
	mediaErrors := uint64(0)
	trimSupported := true
	return domain.Snapshot{
		Generated: now,
		Partial:   true,
		Host: domain.Host{
			Hostname:       "node-a",
			OS:             "Debian GNU/Linux 13",
			Kernel:         "6.14",
			OpenZFSVersion: "zfs-2.3.2",
			ProxmoxVersion: "9.0",
			ProxmoxNode:    "node-a",
		},
		Pools: []domain.Pool{{
			Name: "rpool", GUID: "12345", State: "ONLINE", BootCritical: true,
			AllocatedBytes: 1 << 40, FreeBytes: 1 << 40, SizeBytes: 2 << 40,
			CapacityPercent: 50, FragmentationPct: 7,
			Status: "Some status text", Action: "No action required",
			Scan:  domain.Scan{Function: "scrub", State: "SCANNING", Percent: 50, BytesExamined: 1 << 39, BytesTotal: 1 << 40},
			Trim:  domain.TrimStats{State: "active", Percent: 25, BytesDone: 1 << 38, BytesTotal: 1 << 40, Supported: &trimSupported, ActionTime: now},
			VDevs: []domain.VDev{{Name: "mirror-0", Type: "mirror", Class: "data", State: "ONLINE", Children: []domain.VDev{{Name: "ata-one", Type: "disk", Class: "data", State: "ONLINE", Path: "/dev/disk/by-id/ata-one", SizeBytes: 1 << 40, Trim: domain.TrimStats{State: "active", Percent: 25, BytesDone: 1 << 38, BytesTotal: 1 << 40, Supported: &trimSupported, ActionTime: now}}}}},
		}},
		Drives: []domain.Drive{{
			ID: "disk-one", DevNode: "/dev/sda", ByID: "/dev/disk/by-id/ata-one", Serial: "SERIAL-ONE",
			Vendor: "Example", Model: "Reliable Disk", Firmware: "1.2", CapacityBytes: 1 << 40,
			LogicalSectorBytes: 512, PhysicalSectorBytes: 4096, Transport: "sata", MediaType: "ssd",
			Bay:    domain.Bay{Enclosure: "rack-a", Label: "Bay 01", Source: "manual", Confidence: "verified", VerifiedAt: now},
			Health: domain.DriveHealth{Severity: domain.SeverityOK, SMARTPassed: &passed, TemperatureC: &temp, PercentageUsed: &used, PowerOnHours: &hours, MediaErrors: &mediaErrors, FreshAt: now},
			InUse:  true, Use: "ZFS member of rpool",
		}},
		Datasets: []domain.Dataset{{
			Name: "rpool/data", Type: "filesystem", UsedBytes: 1 << 30, AvailableBytes: 500 << 30, ReferencedBytes: 1 << 30, CompressionRatio: 1.4,
			Properties:      map[string]string{"compression": "lz4", "dedup": "off", "encryption": "off", "recordsize": "128K", "mountpoint": "/rpool/data", "sync": "standard", "atime": "off", "xattr": "sa"},
			PropertySources: map[string]string{"compression": "local", "dedup": "default"},
		}},
		ARC:      domain.ARC{SizeBytes: 4 << 30, TargetBytes: 6 << 30, MaxBytes: 8 << 30, Hits: 80, Misses: 20, L2SizeBytes: 2 << 30, L2Hits: 30, L2Misses: 10},
		Findings: []domain.Finding{{Severity: domain.SeverityWarning, Scope: "pool:rpool", Title: "Example warning", Evidence: "example evidence", Remediation: "example remediation"}},
		Errors:   []domain.SourceError{{Source: "smartctl", Kind: "permission", Message: "one device was unavailable", Partial: true}},
		Commands: []domain.CommandTrace{{Program: "zpool", Args: []string{"status", "-j", "-p"}, Capability: "observe", Risk: domain.RiskRead, StartedAt: now, DurationMS: 12, ExitCode: 0, OutputHidden: true}},
	}
}
