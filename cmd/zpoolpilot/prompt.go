// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func (a *app) confirmYellowContext(ctx context.Context, reason string, argv []string) (bool, error) {
	if !isTTY(a.in) {
		return false, errors.New("changes require a real TTY")
	}
	if len(argv) == 0 {
		return false, errors.New("confirmation requires a command preview")
	}
	line, err := a.readConfirmation(ctx, "%s %s\nReason: %s\nApprove [Y/n]: ", riskLabel(a.color, domain.RiskChange), displayCommand(argv[0], argv[1:]), terminalText(reason))
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func (a *app) promptExact(ctx context.Context, risk domain.Risk, reason, program string, args []string, expected string) (string, error) {
	if !isTTY(a.in) {
		return "", errors.New("apply requires a real TTY")
	}
	line, err := a.readConfirmation(ctx, "\n%s command\n  %s\nReason: %s\nType exactly %q: ", riskLabel(a.color, risk), displayCommand(program, args), terminalText(reason), terminalText(expected))
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), err
}

// readConfirmation requires a visible prompt and a complete line. The CLI's
// signal handler cancels ctx rather than interrupting stdin, so wait for input
// separately and let cancellation terminate the command. No subsequent prompt
// may reuse this reader after cancellation; the CLI exits that command.
func (a *app) readConfirmation(ctx context.Context, format string, args ...any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(a.out, format, args...); err != nil {
		return "", fmt.Errorf("display confirmation: %w", err)
	}
	type response struct {
		line string
		err  error
	}
	done := make(chan response, 1)
	go func() {
		line, err := a.reader.ReadString('\n')
		done <- response{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return result.line, result.err
	}
}

func riskLabel(color bool, risk domain.Risk) string {
	label, code := "GREEN / READ", "1;32"
	if risk == domain.RiskChange {
		label, code = "YELLOW / CHANGE", "1;33"
	} else if risk == domain.RiskDestructive {
		label, code = "RED / DESTRUCTIVE", "1;31"
	}
	if !color {
		return "[" + label + "]"
	}
	return "\x1b[" + code + "m[" + label + "]\x1b[0m"
}

func displayCommand(program string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(program))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	value = terminalText(value)
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>()*?[]{}!#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// terminalText makes untrusted discovery, plan, cache, and error strings safe
// to place next to confirmation prompts. It removes terminal controls and
// Unicode direction overrides instead of trying to recognize escape syntax.
func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x061c || r == 0x200e || r == 0x200f ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '\ufffd'
		}
		return r
	}, value)
}
