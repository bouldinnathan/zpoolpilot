// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAuditFileRejectsSymlinkAndWritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("production root confinement intentionally rejects temporary audit roots")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "audit.jsonl")
	file, err := openAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("safe\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(directory, "audit-link.jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openAuditFile(link); err == nil {
		t.Fatal("audit symlink was accepted")
	}

	writable := filepath.Join(t.TempDir(), "writable")
	if err := os.Mkdir(writable, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := openAuditFile(filepath.Join(writable, "audit.jsonl")); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("unsafe audit directory was accepted: %v", err)
	}
}
