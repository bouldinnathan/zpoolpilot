// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package tui

import (
	"io"
	"os"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

type terminalSession struct {
	input      *os.File
	output     *os.File
	raw        bool
	fullscreen bool
	original   syscall.Termios
	closeOnce  sync.Once
	closeErr   error
}

func openTerminal(in io.Reader, out io.Writer) (*terminalSession, error) {
	s := &terminalSession{}
	inFile, ok := in.(*os.File)
	if !ok || !isTerminalFile(inFile) {
		return s, nil
	}
	s.input = inFile
	if err := ioctl(inFile.Fd(), syscall.TCGETS, unsafe.Pointer(&s.original)); err != nil {
		return nil, err
	}
	raw := s.original
	raw.Lflag &^= syscall.ECHO | syscall.ICANON
	raw.Iflag &^= syscall.ICRNL | syscall.IXON
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1 // deciseconds; bounds cancellation/restore latency
	if err := ioctl(inFile.Fd(), syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	s.raw = true
	if outFile, ok := out.(*os.File); ok {
		s.fullscreen = isTerminalFile(outFile)
		if s.fullscreen {
			s.output = outFile
		}
	}
	return s, nil
}

func (s *terminalSession) enter(w io.Writer) error {
	if !s.fullscreen {
		return nil
	}
	_, err := io.WriteString(w, "\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
	return err
}

func (s *terminalSession) close(w io.Writer) error {
	s.closeOnce.Do(func() {
		if s.fullscreen {
			if _, err := io.WriteString(w, "\x1b[?25h\x1b[?1049l"); err != nil {
				s.closeErr = err
			}
		}
		if s.raw && s.input != nil {
			if err := ioctl(s.input.Fd(), syscall.TCSETS, unsafe.Pointer(&s.original)); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

func (s *terminalSession) size() (int, int) {
	if s.fullscreen && s.output != nil {
		var ws winsize
		if err := ioctl(s.output.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err == nil && ws.Col > 0 && ws.Row > 0 {
			return int(ws.Col), int(ws.Row)
		}
	}
	return envDimension("COLUMNS", 100), envDimension("LINES", 30)
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	var term syscall.Termios
	return ioctl(f.Fd(), syscall.TCGETS, unsafe.Pointer(&term)) == nil
}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func ioctl(fd, request uintptr, value unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func envDimension(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func terminationSignals() []os.Signal {
	// Treat suspend/quit as a controlled dashboard exit. That is safer than
	// handing the shell a process stopped while echo and canonical mode are
	// disabled; the operator can start the dashboard again immediately.
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP}
}

func resizeSignals() []os.Signal { return []os.Signal{syscall.SIGWINCH} }
