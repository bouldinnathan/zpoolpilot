// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireExcludesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collect.lock")
	first, err := Acquire(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), path, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire=%v, want ErrBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Acquire(context.Background(), path, false)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = third.Close()
}

func TestAcquireRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "collect.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), link, false); err == nil {
		t.Fatal("symlink lock was accepted")
	}
}

func TestAcquireWaitHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collect.lock")
	first, err := Acquire(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path, true); !errors.Is(err, ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait acquire=%v, want busy+deadline", err)
	}
}

func TestAcquireCanceledContextDoesNotCreateLock(t *testing.T) {
	for _, wait := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "new-directory", "collect.lock")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if lock, err := Acquire(ctx, path, wait); !errors.Is(err, context.Canceled) || lock != nil {
			t.Fatalf("Acquire(wait=%v) = %v, %v; want canceled without lock", wait, lock, err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled acquire created directory: %v", err)
		}
	}
}
