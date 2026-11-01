// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

func TestFallbackRoundTrip(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	s.ForceFallback()
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := struct {
		Name string `json:"name"`
	}{Name: "round-trip"}
	if err := s.Put(context.Background(), "test/value", want); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name string `json:"name"`
	}
	err := s.Get(context.Background(), "test/value", &got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name {
		t.Fatalf("got %+v", got)
	}
}

func TestFallbackRejectsSymlinkAndOversizedState(t *testing.T) {
	directory := t.TempDir()
	s := Open(filepath.Join(directory, "state.db"))
	s.ForceFallback()
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte(`{"schema":1,"values":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.fallbackPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readFallback(); err == nil {
		t.Fatal("fallback state symlink was accepted")
	}
	if err := os.Remove(s.fallbackPath()); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(s.fallbackPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxFallbackStateBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readFallback(); err == nil {
		t.Fatal("oversized fallback state was accepted")
	}
}

func TestFallbackSerializesIndependentStoreInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	stores := []*Store{Open(path), Open(path)}
	for _, item := range stores {
		item.ForceFallback()
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for index := 0; index < 40; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := fmt.Sprintf("disk-%02d", index)
			errs <- stores[index%len(stores)].Put(ctx, key, key)
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 40; index++ {
		key := fmt.Sprintf("disk-%02d", index)
		var value string
		err := stores[0].Get(ctx, key, &value)
		if err != nil || value != key {
			t.Fatalf("load %s: value=%q err=%v", key, value, err)
		}
	}
}

func TestEnsureSharedStateFilePermissionsAndSymlinkRejection(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.db")
	if err := ensureSharedStateFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("state permissions=%04o, want 0660", info.Mode().Perm())
	}
	link := filepath.Join(directory, "state-link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureSharedStateFile(link); err == nil {
		t.Fatal("state database symlink was accepted")
	}
}

func TestSQLiteRoundTripWhenAvailable(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	if s.sqlitePath == "" {
		t.Skip("sqlite3 not available")
	}
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	want := domain.Snapshot{APIVersion: domain.SnapshotAPIVersion, Generated: time.Now().UTC(), Host: domain.Host{Hostname: "node1"}}
	if err := s.SaveSnapshot(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host.Hostname != "node1" {
		t.Fatalf("got %+v", got)
	}
	mode, err := s.sqlite(ctx, "PRAGMA journal_mode;\n")
	if err != nil || strings.TrimSpace(mode) != "memory" {
		t.Fatalf("journal mode=%q err=%v, want descriptor-safe memory", mode, err)
	}
	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(s.path) {
			t.Fatalf("unexpected SQLite sidecar %q", entry.Name())
		}
	}
}

func TestSQLiteFailsClosedWhenOutputExceedsLimit(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	if s.sqlitePath == "" {
		t.Skip("sqlite3 not available")
	}
	s.maxStdoutBytes = 32
	s.maxStderrBytes = 32
	_, err := s.sqlite(context.Background(), "SELECT printf('%128s', 'x');\n")
	if !errors.Is(err, execx.ErrOutputLimit) {
		t.Fatalf("sqlite() error = %v, want ErrOutputLimit", err)
	}
}

func TestSQLitePreservesCallerCancellation(t *testing.T) {
	directory := t.TempDir()
	started := filepath.Join(directory, "started")
	helper := filepath.Join(directory, "blocking-sqlite")
	script := "#!/bin/sh\n: > " + started + "\nexec sleep 30\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Open(filepath.Join(directory, "state.db"))
	s.sqlitePath = helper
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	_, err := s.sqlite(ctx, "SELECT 1;\n")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sqlite cancellation error = %v, want context.Canceled", err)
	}
}

func TestFallbackCanceledOperationsDoNotCreateState(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "missing", "state.db"))
	s.ForceFallback()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var value string
	for name, operation := range map[string]func() error{
		"init": func() error { return s.Init(ctx) },
		"put":  func() error { return s.Put(ctx, "key", "value") },
		"get":  func() error { return s.Get(ctx, "key", &value) },
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled %s returned %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(s.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled operations created state directory: %v", err)
	}
}

func TestFallbackRejectsIncompleteEnvelope(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	s.ForceFallback()
	for _, document := range []string{"null", "{}", `{"values":{}}`, `{"schema":1}`, `{"schema":1,"values":null}`} {
		if err := os.WriteFile(s.fallbackPath(), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(context.Background()); err == nil {
			t.Errorf("incomplete fallback envelope accepted: %s", document)
		}
	}
}

func TestFallbackOversizedWritePreservesPreviousState(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	s.ForceFallback()
	ctx := context.Background()
	if err := s.Put(ctx, "key", "previous"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "key", strings.Repeat("x", maxFallbackStateBytes)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized fallback write returned %v", err)
	}
	var got string
	if err := s.Get(ctx, "key", &got); err != nil || got != "previous" {
		t.Fatalf("oversized write replaced readable state: value=%q error=%v", got, err)
	}
}

func TestSQLiteOversizedValueCannotReplaceReadableState(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	if s.sqlitePath == "" {
		t.Skip("sqlite3 not available")
	}
	s.maxStdoutBytes = 32
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "key", "previous"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "key", strings.Repeat("x", 30)); !errors.Is(err, execx.ErrOutputLimit) {
		t.Fatalf("unreadable SQLite write returned %v", err)
	}
	var got string
	if err := s.Get(ctx, "key", &got); err != nil || got != "previous" {
		t.Fatalf("unreadable write replaced previous value: value=%q error=%v", got, err)
	}
}

func TestSQLiteInitRejectsUnsupportedSchemaWithoutRewritingIt(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	if s.sqlitePath == "" {
		t.Skip("sqlite3 not available")
	}
	ctx := context.Background()
	_, err := s.sqlite(ctx, "CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO metadata VALUES('schema_version','999');\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err == nil || !strings.Contains(err.Error(), "unsupported state schema") {
		t.Fatalf("unsupported schema was accepted: %v", err)
	}
	version, err := s.sqlite(ctx, "SELECT value FROM metadata WHERE key='schema_version';\n")
	if err != nil || strings.TrimSpace(version) != "999" {
		t.Fatalf("unsupported schema marker was overwritten: value=%q error=%v", version, err)
	}
}
