// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package execx runs the small, explicitly allowlisted set of external
// programs used by zpoolpilot. Commands are always represented as an executable
// and an argument vector; this package never invokes a shell.
package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
)

// Capability separates commands which only observe the host from commands
// which can change a pool, device, or host configuration.
type Capability string

const (
	CapabilityObserve Capability = "observe"
	CapabilityMutate  Capability = "mutate"
)

const DefaultTimeout = 30 * time.Second

const maxStderrDetailBytes = 240

// TrustedPath is the complete executable search path used by subprocesses.
// It deliberately excludes caller-controlled and local installation paths.
const TrustedPath = "/usr/sbin:/usr/bin:/sbin:/bin"

var trustedExecutableDirectories = strings.Split(TrustedPath, ":")

var (
	ErrInvalidCommand = errors.New("invalid command")
	ErrNotAllowed     = errors.New("command is not allowlisted")
	ErrReadOnly       = errors.New("mutation rejected in read-only mode")
	ErrTimedOut       = errors.New("command timed out")
	ErrOutputLimit    = errors.New("command output exceeded capture limit")
)

// Command is an argv-only request. Program must be a bare executable name;
// paths, shell fragments, and NUL bytes are rejected before execution.
type Command struct {
	Program    string
	Args       []string
	Capability Capability
	Risk       domain.Risk
	Timeout    time.Duration
	Stdin      io.Reader
	// Interactive attaches the child directly to the process terminal. Its
	// output is not captured, and the trace is forcibly marked hidden. This is
	// intended for native secret prompts, never ordinary observation.
	Interactive  bool
	OutputHidden bool
}

// Result keeps command output separate and includes the redaction-safe trace.
type Result struct {
	Stdout []byte
	Stderr []byte
	Trace  domain.CommandTrace
}

// Runner is implemented by OSRunner and FakeRunner.
type Runner interface {
	Run(context.Context, Command) (Result, error)
}

// Rule permits a program when Match accepts its complete argument vector.
// Match must be deterministic and must not have side effects.
type Rule struct {
	Program string
	Match   func([]string) bool
}

// Allowlist contains independent observe and mutate rules. A command must
// match a rule in the list for its declared capability.
type Allowlist struct {
	Observe []Rule
	Mutate  []Rule
}

// Validate rejects malformed and unapproved commands.
func (a Allowlist) Validate(command Command) error {
	if err := validateShape(command); err != nil {
		return err
	}
	var rules []Rule
	switch command.Capability {
	case CapabilityObserve:
		rules = a.Observe
	case CapabilityMutate:
		rules = a.Mutate
	default:
		return fmt.Errorf("%w: unknown capability %q", ErrInvalidCommand, command.Capability)
	}
	for _, rule := range rules {
		if rule.Program == command.Program && rule.Match != nil && rule.Match(command.Args) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s for %s", ErrNotAllowed, command.Program, command.Capability)
}

func validateShape(command Command) error {
	if command.Program == "" || filepath.Base(command.Program) != command.Program ||
		strings.ContainsAny(command.Program, "/\\\x00\r\n\t ") || command.Program == "." || command.Program == ".." {
		return fmt.Errorf("%w: program must be a bare executable name", ErrInvalidCommand)
	}
	for _, arg := range command.Args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("%w: argument contains NUL", ErrInvalidCommand)
		}
	}
	if command.Risk != domain.RiskRead && command.Risk != domain.RiskChange && command.Risk != domain.RiskDestructive {
		return fmt.Errorf("%w: risk must be green, yellow, or red", ErrInvalidCommand)
	}
	if command.Capability == CapabilityObserve && command.Risk != domain.RiskRead {
		return fmt.Errorf("%w: observe command must have read risk", ErrInvalidCommand)
	}
	if command.Capability == CapabilityMutate && command.Risk == domain.RiskRead {
		return fmt.Errorf("%w: mutation cannot have read risk", ErrInvalidCommand)
	}
	if command.Timeout < 0 {
		return fmt.Errorf("%w: timeout cannot be negative", ErrInvalidCommand)
	}
	if command.Interactive && (command.Capability != CapabilityMutate || command.Risk != domain.RiskDestructive || command.Stdin != nil) {
		return fmt.Errorf("%w: interactive mode requires a red mutation and inherited terminal input", ErrInvalidCommand)
	}
	return nil
}

func exactArgv(options ...[]string) func([]string) bool {
	return func(args []string) bool {
		for _, option := range options {
			if slices.Equal(args, option) {
				return true
			}
		}
		return false
	}
}

func anyArgs([]string) bool { return true }

func smartctlObserve(args []string) bool {
	if len(args) == 3 {
		return args[0] == "--json=c" && args[1] == "--all" && observableDevicePath(args[2])
	}
	if len(args) == 5 {
		return args[0] == "--json=c" && args[1] == "--all" &&
			args[2] == "--device" && args[3] == "nvme" && observableDevicePath(args[4])
	}
	return len(args) == 7 && args[0] == "--json=c" && args[1] == "--all" &&
		args[2] == "--device" && (args[3] == "sat" || args[3] == "ata") &&
		args[4] == "--nocheck" && args[5] == "standby,2,2" && observableDevicePath(args[6])
}

func observableDevicePath(path string) bool {
	return strings.HasPrefix(path, "/dev/") && path != "/dev/" && filepath.Clean(path) == path &&
		!strings.ContainsAny(path, "\x00\r\n\t")
}

func udevInfoObserve(args []string) bool {
	return len(args) == 4 && args[0] == "info" && args[1] == "--query=property" &&
		args[2] == "--name" && observableDevicePath(args[3])
}

func sgSESMutate(args []string) bool {
	if len(args) != 3 || !strings.HasPrefix(args[0], "--dev-slot-num=") ||
		(args[1] != "--set=ident" && args[1] != "--clear=ident") || !inventory.ValidSESDevicePath(args[2]) {
		return false
	}
	slot := strings.TrimPrefix(args[0], "--dev-slot-num=")
	return slot != "" && !strings.ContainsAny(slot, "+-") && strings.Trim(slot, "0123456789") == ""
}

func safeDiskMutation(args []string) bool {
	return len(args) == 2 && args[0] == "--all" && wholeDiskPath(args[1])
}

func wholeDiskPath(path string) bool {
	if filepath.Dir(path) != "/dev/disk/by-id" || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n\t") {
		return false
	}
	base := filepath.Base(path)
	marker := strings.LastIndex(base, "-part")
	if marker < 0 {
		return true
	}
	suffix := base[marker+len("-part"):]
	return suffix == "" || strings.Trim(suffix, "0123456789") != ""
}

func fixedPartitionPath(path, partition string) bool {
	suffix := "-part" + partition
	return strings.HasSuffix(path, suffix) && wholeDiskPath(strings.TrimSuffix(path, suffix))
}

func sgdiskMutation(args []string) bool {
	if len(args) == 2 && args[0] == "--zap-all" {
		return wholeDiskPath(args[1])
	}
	if len(args) == 2 && args[0] == "-G" {
		return wholeDiskPath(args[1])
	}
	return len(args) == 3 && wholeDiskPath(args[0]) && args[1] == "-R" &&
		wholeDiskPath(args[2]) && args[0] != args[2]
}

func proxmoxBootMutation(args []string) bool {
	if len(args) == 2 && args[0] == "format" {
		return fixedPartitionPath(args[1], "2")
	}
	if len(args) == 2 && args[0] == "init" {
		return fixedPartitionPath(args[1], "2")
	}
	return len(args) == 3 && args[0] == "init" && fixedPartitionPath(args[1], "2") && args[2] == "grub"
}

func zpoolMutation(args []string) bool {
	if len(args) == 0 || !slices.Contains([]string{"create", "add", "replace", "attach", "clear", "scrub", "trim"}, args[0]) {
		return false
	}
	// Routine maintenance is intentionally limited to the five exact argv
	// forms emitted by planner.RebuildCommands. In particular, recovery flags
	// such as `zpool clear -F` and scrub/trim options cannot pass this layer.
	if args[0] == "clear" {
		return len(args) == 2 && safeZpoolTarget(args[1])
	}
	if args[0] == "scrub" || args[0] == "trim" {
		return (len(args) == 2 && safeZpoolTarget(args[1])) ||
			(len(args) == 3 && args[1] == "-s" && safeZpoolTarget(args[2]))
	}
	for _, arg := range args[1:] {
		if arg == "-f" || arg == "--force" {
			return false
		}
		if strings.HasPrefix(arg, "/dev/") && !strings.HasPrefix(arg, "/dev/disk/by-id/") {
			return false
		}
	}
	return true
}

func safeZpoolTarget(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, "/\\\x00\r\n\t ")
}

// DefaultAllowlist is intentionally narrow. Argument-level invariants for
// destructive operations are checked again by planner.Apply before dispatch.
func DefaultAllowlist() Allowlist {
	const lsblkColumns = "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,VENDOR,REV,ROTA,TRAN,LOG-SEC,PHY-SEC,MOUNTPOINTS,FSTYPE,PKNAME"
	return Allowlist{
		Observe: []Rule{
			{Program: "zpool", Match: exactArgv(
				[]string{"version", "-j"},
				[]string{"--version"},
				[]string{"status", "-j", "--json-int", "-P", "-t"},
				[]string{"status", "-P", "-p", "-t"},
				[]string{"list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"},
				[]string{"list", "-H", "-p", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"},
				[]string{"get", "-j", "--json-int", "all"},
				[]string{"get", "-H", "-p", "-o", "name,property,value,source", "all"},
				[]string{"iostat", "-H", "-p"},
			)},
			{Program: "zfs", Match: exactArgv(
				[]string{"get", "-j", "--json-int", "all"},
				[]string{"get", "-H", "-p", "-o", "name,property,value,source", "all"},
			)},
			{Program: "lsblk", Match: exactArgv([]string{"--json", "--bytes", "--paths", "--output", lsblkColumns})},
			{Program: "udevadm", Match: udevInfoObserve},
			{Program: "smartctl", Match: smartctlObserve},
			{Program: "pveversion", Match: exactArgv([]string{})},
			{Program: "proxmox-boot-tool", Match: exactArgv([]string{"status"})},
		},
		Mutate: []Rule{
			{Program: "zpool", Match: zpoolMutation},
			{Program: "wipefs", Match: safeDiskMutation},
			{Program: "sgdisk", Match: sgdiskMutation},
			{Program: "partprobe", Match: func(args []string) bool { return len(args) == 1 && wholeDiskPath(args[0]) }},
			{Program: "sg_ses", Match: sgSESMutate},
			{Program: "proxmox-boot-tool", Match: proxmoxBootMutation},
		},
	}
}

// Config controls an OSRunner. A zero Allowlist uses DefaultAllowlist.
type Config struct {
	ReadOnly       bool
	DefaultTimeout time.Duration
	MaxStdoutBytes int64
	MaxStderrBytes int64
	Allowlist      Allowlist
	OnTrace        func(domain.CommandTrace)
}

// OSRunner dispatches approved commands directly through os/exec.
type OSRunner struct {
	readOnly       bool
	defaultTimeout time.Duration
	maxStdoutBytes int64
	maxStderrBytes int64
	allowlist      Allowlist
	onTrace        func(domain.CommandTrace)

	mu     sync.Mutex
	traces []domain.CommandTrace
}

func NewOSRunner(config Config) *OSRunner {
	allowlist := config.Allowlist
	if len(allowlist.Observe) == 0 && len(allowlist.Mutate) == 0 {
		allowlist = DefaultAllowlist()
	}
	timeout := config.DefaultTimeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &OSRunner{
		readOnly:       config.ReadOnly,
		defaultTimeout: timeout,
		maxStdoutBytes: positiveOrDefault(config.MaxStdoutBytes, DefaultMaxStdoutBytes),
		maxStderrBytes: positiveOrDefault(config.MaxStderrBytes, DefaultMaxStderrBytes),
		allowlist:      allowlist,
		onTrace:        config.OnTrace,
	}
}

func (r *OSRunner) Run(ctx context.Context, command Command) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	if err := r.allowlist.Validate(command); err != nil {
		return Result{}, err
	}
	if r.readOnly && command.Capability == CapabilityMutate {
		return Result{}, ErrReadOnly
	}

	timeout := command.Timeout
	if timeout == 0 {
		timeout = r.defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now().UTC()
	executable, resolveErr := ResolveProgram(command.Program)
	if resolveErr != nil {
		trace := domain.CommandTrace{
			Program: command.Program, Args: slices.Clone(command.Args), Capability: string(command.Capability),
			Risk: command.Risk, StartedAt: started, ExitCode: -1, Error: resolveErr.Error(),
		}
		r.record(trace)
		return Result{Trace: trace}, resolveErr
	}
	cmd := exec.CommandContext(runCtx, executable, command.Args...)
	cmd.Env = localeEnvironment()
	// Observation tools and noninteractive helpers may spawn subprocesses.
	// Keep those in a dedicated group so a deadline stops the whole operation,
	// including descendants retaining the output pipes. Interactive commands
	// must retain the terminal's foreground group for native prompts.
	if !command.Interactive {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.WaitDelay = time.Second
	}
	stdout := NewBoundedBuffer(r.maxStdoutBytes)
	stderr := NewBoundedBuffer(r.maxStderrBytes)
	if command.Interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdin = command.Stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}

	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) && !command.Interactive && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if !command.Interactive {
		runErr = errors.Join(runErr, stdout.LimitError("stdout"), stderr.LimitError("stderr"))
	}
	duration := time.Since(started)
	trace := domain.CommandTrace{
		Program:      command.Program,
		Args:         slices.Clone(command.Args),
		Capability:   string(command.Capability),
		Risk:         command.Risk,
		StartedAt:    started,
		DurationMS:   duration.Milliseconds(),
		ExitCode:     exitCode(runErr),
		OutputHidden: command.OutputHidden || command.Interactive,
	}
	// A caller-owned context ending is an operation cancellation, not a
	// command timeout. Check the parent first so its cause survives even though
	// the derived timeout context reports the same DeadlineExceeded sentinel.
	if parentErr := ctx.Err(); parentErr != nil {
		trace.TimedOut = errors.Is(parentErr, context.DeadlineExceeded)
		trace.Error = parentErr.Error()
		runErr = errors.Join(runErr, parentErr)
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		trace.TimedOut = true
		trace.Error = ErrTimedOut.Error()
		runErr = fmt.Errorf("%w: %s", ErrTimedOut, command.Program)
	} else if errors.Is(runCtx.Err(), context.Canceled) {
		trace.Error = context.Canceled.Error()
		runErr = errors.Join(runErr, context.Canceled)
	} else if runErr != nil {
		trace.Error = runErr.Error()
	}
	r.record(trace)
	stdoutBytes, stderrBytes := stdout.Bytes(), stderr.Bytes()
	if command.Interactive || command.OutputHidden {
		stdoutBytes, stderrBytes = nil, nil
	}
	return Result{Stdout: stdoutBytes, Stderr: stderrBytes, Trace: trace}, withStderrDetail(runErr, stderr.Bytes(), command.Interactive || command.OutputHidden)
}

func positiveOrDefault(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func withStderrDetail(err error, stderr []byte, hidden bool) error {
	if err == nil || hidden {
		return err
	}
	detail := boundedOneLine(stderr)
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: stderr: %s", err, detail)
}

func boundedOneLine(value []byte) string {
	line := string(value)
	if index := strings.IndexAny(line, "\r\n"); index >= 0 {
		line = line[:index]
	}
	line = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, strings.ToValidUTF8(line, "�"))
	line = strings.Join(strings.Fields(line), " ")
	if len(line) <= maxStderrDetailBytes {
		return line
	}
	limit := maxStderrDetailBytes - len("…")
	end := 0
	for index, character := range line {
		size := utf8.RuneLen(character)
		if index+size > limit {
			break
		}
		end = index + size
	}
	return line[:end] + "…"
}

func localeEnvironment() []string {
	environment := []string{"PATH=" + TrustedPath, "LC_ALL=C", "LANG=C", "TZ=UTC"}
	if term := os.Getenv("TERM"); term != "" && !strings.ContainsAny(term, "\x00\r\n") {
		environment = append(environment, "TERM="+term)
	}
	return environment
}

// ResolveProgram ignores the process PATH and resolves a bare program name
// only from root-controlled system directories used by supported Linux hosts.
func ResolveProgram(program string) (string, error) {
	if program == "" || filepath.Base(program) != program ||
		strings.ContainsAny(program, "/\\\x00\r\n\t ") || program == "." || program == ".." {
		return "", fmt.Errorf("%w: program must be a bare executable name", ErrInvalidCommand)
	}
	for _, directory := range trustedExecutableDirectories {
		candidate := filepath.Join(directory, program)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: %s is absent from trusted PATH %s", exec.ErrNotFound, program, TrustedPath)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (r *OSRunner) record(trace domain.CommandTrace) {
	trace.Args = slices.Clone(trace.Args)
	r.mu.Lock()
	r.traces = append(r.traces, trace)
	r.mu.Unlock()
	if r.onTrace != nil {
		trace.Args = slices.Clone(trace.Args)
		r.onTrace(trace)
	}
}

// Traces returns a defensive copy of all attempted, dispatched commands.
// Commands rejected before dispatch are deliberately absent.
func (r *OSRunner) Traces() []domain.CommandTrace {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]domain.CommandTrace, len(r.traces))
	copy(result, r.traces)
	for index := range result {
		result[index].Args = slices.Clone(result[index].Args)
	}
	return result
}

// ResetTraces clears captured traces.
func (r *OSRunner) ResetTraces() {
	r.mu.Lock()
	r.traces = nil
	r.mu.Unlock()
}
