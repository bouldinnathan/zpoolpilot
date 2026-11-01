// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux

package terminal

import "os"

// IsTerminal fails closed on unsupported hosts. ZpoolPilot's mutation
// workflows target Linux and must never infer a terminal from file type alone.
func IsTerminal(_ *os.File) bool { return false }
