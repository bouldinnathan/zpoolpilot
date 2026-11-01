// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

// Package terminal contains the minimal kernel-level terminal check shared by
// mutation gates. A character-device mode bit is insufficient because devices
// such as /dev/null and /dev/urandom are not interactive terminals.
package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether file accepts the Linux terminal TCGETS ioctl.
func IsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	var state syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&state)))
	return errno == 0
}
