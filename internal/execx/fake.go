// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package execx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

var ErrNoFakeResponse = errors.New("no fake response configured")

// FakeResponse is consumed in FIFO order by FakeRunner.
type FakeResponse struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Delay    time.Duration
	Err      error
}

// FakeRunner is a concurrency-safe Runner for package and integration tests.
// It applies the same shape, allowlist, and read-only checks as OSRunner.
type FakeRunner struct {
	ReadOnly  bool
	Allowlist Allowlist

	mu        sync.Mutex
	responses []FakeResponse
	calls     []Command
	traces    []domain.CommandTrace
}

func NewFakeRunner(readOnly bool, responses ...FakeResponse) *FakeRunner {
	return &FakeRunner{
		ReadOnly:  readOnly,
		Allowlist: DefaultAllowlist(),
		responses: slices.Clone(responses),
	}
}

func (f *FakeRunner) Queue(responses ...FakeResponse) {
	f.mu.Lock()
	f.responses = append(f.responses, responses...)
	f.mu.Unlock()
}

func (f *FakeRunner) Run(ctx context.Context, command Command) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	allowlist := f.Allowlist
	if len(allowlist.Observe) == 0 && len(allowlist.Mutate) == 0 {
		allowlist = DefaultAllowlist()
	}
	if err := allowlist.Validate(command); err != nil {
		return Result{}, err
	}
	if f.ReadOnly && command.Capability == CapabilityMutate {
		return Result{}, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return f.finish(command, time.Now().UTC(), nil, nil, -1, errors.Is(err, context.DeadlineExceeded), err)
	}
	runCtx := ctx
	if command.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, command.Timeout)
		defer cancel()
	}

	f.mu.Lock()
	f.calls = append(f.calls, cloneCommand(command))
	if len(f.responses) == 0 {
		f.mu.Unlock()
		return Result{}, ErrNoFakeResponse
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	f.mu.Unlock()

	started := time.Now().UTC()
	if response.Delay > 0 {
		timer := time.NewTimer(response.Delay)
		defer timer.Stop()
		select {
		case <-runCtx.Done():
			timeoutErr := runCtx.Err()
			if errors.Is(timeoutErr, context.DeadlineExceeded) && ctx.Err() == nil {
				timeoutErr = ErrTimedOut
			}
			return f.finish(command, started, nil, nil, -1, errors.Is(timeoutErr, context.DeadlineExceeded) || errors.Is(timeoutErr, ErrTimedOut), timeoutErr)
		case <-timer.C:
		}
	}
	return f.finish(command, started, response.Stdout, response.Stderr, response.ExitCode, false, response.Err)
}

func (f *FakeRunner) finish(command Command, started time.Time, stdout, stderr []byte, code int, timedOut bool, err error) (Result, error) {
	if command.Interactive || command.OutputHidden {
		stdout, stderr = nil, nil
	}
	trace := domain.CommandTrace{
		Program:      command.Program,
		Args:         slices.Clone(command.Args),
		Capability:   string(command.Capability),
		Risk:         command.Risk,
		StartedAt:    started,
		DurationMS:   time.Since(started).Milliseconds(),
		ExitCode:     code,
		TimedOut:     timedOut,
		OutputHidden: command.OutputHidden || command.Interactive,
	}
	if err != nil {
		trace.Error = err.Error()
	}
	f.mu.Lock()
	f.traces = append(f.traces, trace)
	f.mu.Unlock()
	return Result{Stdout: slices.Clone(stdout), Stderr: slices.Clone(stderr), Trace: trace}, withStderrDetail(err, stderr, command.Interactive || command.OutputHidden)
}

func cloneCommand(command Command) Command {
	command.Args = slices.Clone(command.Args)
	return command
}

func (f *FakeRunner) Calls() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]Command, len(f.calls))
	for index, command := range f.calls {
		result[index] = cloneCommand(command)
	}
	return result
}

func (f *FakeRunner) Traces() []domain.CommandTrace {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]domain.CommandTrace, len(f.traces))
	copy(result, f.traces)
	for index := range result {
		result[index].Args = slices.Clone(result[index].Args)
	}
	return result
}
