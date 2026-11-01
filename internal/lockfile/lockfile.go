// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package lockfile provides a small Linux advisory lock used to serialize
// snapshot-cache replacement. It never removes lock files, avoiding inode
// replacement races between long-lived services.
package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ErrBusy = errors.New("lock is held by another process")

type Lock struct {
	file *os.File
}

func Acquire(ctx context.Context, path string, wait bool) (*Lock, error) {
	if ctx == nil {
		return nil, errors.New("nil lock context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("lock path must be absolute and clean")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o660)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeWith := func(err error) (*Lock, error) {
		_ = file.Close()
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return closeWith(fmt.Errorf("stat lock: %w", err))
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		return closeWith(errors.New("lock must be a singly linked regular file"))
	}
	// The packaged lock directory is setgid zpoolpilot and both collector users
	// need read/write access regardless of their service umask.
	if stat.Mode&0o660 != 0o660 {
		if err := syscall.Fchmod(fd, 0o660); err != nil {
			return closeWith(fmt.Errorf("set lock permissions: %w", err))
		}
	}
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := ctx.Err(); err != nil {
				return closeWith(err)
			}
			return &Lock{file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return closeWith(fmt.Errorf("acquire lock: %w", err))
		}
		if !wait {
			return closeWith(ErrBusy)
		}
		select {
		case <-ctx.Done():
			return closeWith(errors.Join(ErrBusy, ctx.Err()))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (lock *Lock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	fd := int(lock.file.Fd())
	err := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(err, closeErr)
}
