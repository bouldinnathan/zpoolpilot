// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux

package tui

import (
	"io"
	"os"
	"strconv"
)

// ZpoolPilot targets Linux/OpenZFS. The non-Linux implementation keeps tests
// and read-only summaries portable by using the line-oriented input fallback.
type terminalSession struct {
	input      *os.File
	output     *os.File
	raw        bool
	fullscreen bool
}

func openTerminal(_ io.Reader, _ io.Writer) (*terminalSession, error) {
	return &terminalSession{}, nil
}
func (s *terminalSession) enter(_ io.Writer) error { return nil }
func (s *terminalSession) close(_ io.Writer) error { return nil }
func (s *terminalSession) size() (int, int) {
	return envDimension("COLUMNS", 100), envDimension("LINES", 30)
}
func isTerminalFile(_ *os.File) bool { return false }
func envDimension(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt} }
func resizeSignals() []os.Signal      { return nil }
