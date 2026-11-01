// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestTerminalSessionRestoresTermiosAndScreen(t *testing.T) {
	master, slave, original := openPTY(t)
	defer master.Close()
	defer slave.Close()

	session, err := openTerminal(slave, slave)
	if err != nil {
		t.Fatal(err)
	}
	if !session.raw || !session.fullscreen {
		t.Fatalf("session raw=%v fullscreen=%v", session.raw, session.fullscreen)
	}
	current := readTermios(t, slave)
	if current.Lflag&syscall.ICANON != 0 || current.Lflag&syscall.ECHO != 0 {
		t.Fatalf("terminal was not put in single-key mode: lflag=%#x", current.Lflag)
	}

	var controls bytes.Buffer
	if err := session.enter(&controls); err != nil {
		t.Fatal(err)
	}
	if err := session.close(&controls); err != nil {
		t.Fatal(err)
	}
	// Close is deliberately idempotent because multiple shutdown causes can
	// become ready at the same time.
	if err := session.close(&controls); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\x1b[?1049h", "\x1b[?25l", "\x1b[?25h", "\x1b[?1049l"} {
		if !strings.Contains(controls.String(), want) {
			t.Errorf("missing screen control %q in %q", want, controls.String())
		}
	}
	assertTermiosEqual(t, readTermios(t, slave), original)
}

func TestRunRawSingleKeyRestoresOnQuit(t *testing.T) {
	master, slave, original := openPTY(t)
	defer master.Close()
	defer slave.Close()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), domain.Snapshot{}, Options{In: slave, Out: io.Discard})
	}()
	waitForRaw(t, slave)
	if _, err := master.Write([]byte{'q'}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not accept a single key without Enter")
	}
	assertTermiosEqual(t, readTermios(t, slave), original)
}

func TestRunRestoresTerminalOnContextCancellation(t *testing.T) {
	master, slave, original := openPTY(t)
	defer master.Close()
	defer slave.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, domain.Snapshot{}, Options{In: slave, Out: io.Discard})
	}()
	waitForRaw(t, slave)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	assertTermiosEqual(t, readTermios(t, slave), original)
}

func TestRunRestoresTerminalOnSignal(t *testing.T) {
	master, slave, original := openPTY(t)
	defer master.Close()
	defer slave.Close()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), domain.Snapshot{}, Options{In: slave, Out: io.Discard})
	}()
	waitForRaw(t, slave)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	assertTermiosEqual(t, readTermios(t, slave), original)
}

func openPTY(t *testing.T) (*os.File, *os.File, syscall.Termios) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo-terminal unavailable: %v", err)
	}
	var unlock int32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		t.Skipf("cannot unlock pseudo-terminal: %v", err)
	}
	var number uint32
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		master.Close()
		t.Skipf("cannot identify pseudo-terminal: %v", err)
	}
	slave, err := os.OpenFile("/dev/pts/"+fmtUint(number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("cannot open pseudo-terminal slave: %v", err)
	}
	return master, slave, readTermios(t, slave)
}

func readTermios(t *testing.T, file *os.File) syscall.Termios {
	t.Helper()
	var term syscall.Termios
	if err := ioctl(file.Fd(), syscall.TCGETS, unsafe.Pointer(&term)); err != nil {
		t.Fatal(err)
	}
	return term
}

func waitForRaw(t *testing.T, file *os.File) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		term := readTermios(t, file)
		if term.Lflag&syscall.ICANON == 0 && term.Lflag&syscall.ECHO == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("terminal did not enter raw single-key mode")
}

func assertTermiosEqual(t *testing.T, got, want syscall.Termios) {
	t.Helper()
	if got != want {
		t.Fatalf("terminal settings not restored:\n got  %#v\n want %#v", got, want)
	}
}

func fmtUint(value uint32) string {
	if value == 0 {
		return "0"
	}
	var digits [10]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
