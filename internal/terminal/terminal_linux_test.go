// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

func TestIsTerminalRejectsCharacterDeviceAndAcceptsPTY(t *testing.T) {
	null, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if IsTerminal(nil) || IsTerminal(null) {
		t.Fatal("nil or /dev/null was accepted as a real terminal")
	}

	master, slave := openTestPTY(t)
	defer master.Close()
	defer slave.Close()
	if !IsTerminal(master) || !IsTerminal(slave) {
		t.Fatal("PTY master or slave was not recognized as a terminal")
	}
}

func openTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open /dev/ptmx: %v", err)
	}
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		t.Skipf("unlock PTY: %v", errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		master.Close()
		t.Skipf("read PTY number: %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("open PTY slave: %v", err)
	}
	return master, slave
}
