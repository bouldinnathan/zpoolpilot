// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/lockfile"
)

const (
	schemaVersion         = 1
	maxFallbackStateBytes = 64 << 20
)

type Store struct {
	path           string
	sqlitePath     string
	maxStdoutBytes int64
	maxStderrBytes int64
	mu             sync.Mutex
}

type fallbackState struct {
	Schema int                        `json:"schema"`
	Values map[string]json.RawMessage `json:"values"`
}

func Open(path string) *Store {
	sqlite, _ := execx.ResolveProgram("sqlite3")
	return &Store{
		path: path, sqlitePath: sqlite,
		maxStdoutBytes: execx.DefaultMaxStdoutBytes,
		maxStderrBytes: execx.DefaultMaxStderrBytes,
	}
}

// ForceFallback is intended for deterministic tests and constrained systems.
func (s *Store) ForceFallback() { s.sqlitePath = "" }

func (s *Store) Init(ctx context.Context) error {
	if err := validContext(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	if s.sqlitePath == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, err := s.readFallback()
		return err
	}
	if err := ensureSharedStateFile(s.path); err != nil {
		return err
	}
	schema := `CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO metadata(key,value) VALUES('schema_version','1') ON CONFLICT(key) DO NOTHING;
SELECT value FROM metadata WHERE key='schema_version';
`
	version, err := s.sqlite(ctx, schema)
	if err != nil {
		return err
	}
	if strings.TrimSpace(version) != fmt.Sprint(schemaVersion) {
		return fmt.Errorf("unsupported state schema %q", strings.TrimSpace(version))
	}
	_, err = s.sqlite(ctx, "CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL);\n")
	return err
}

func (s *Store) Put(ctx context.Context, key string, value any) error {
	if err := validContext(ctx); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if s.sqlitePath == "" {
		return s.putFallback(ctx, key, b)
	}
	// Get captures the MEMORY journal-mode line, the base64 value, and its
	// trailing newline. Refuse writes that would become unreadable through the
	// same store's output cap, preserving any previous value for this key.
	encodedSize := int64(base64.StdEncoding.EncodedLen(len(b)))
	if encodedSize+int64(len("memory\n\n")) > s.maxStdoutBytes {
		return fmt.Errorf("%w: encoded state value exceeds SQLite read capture limit of %d bytes", execx.ErrOutputLimit, s.maxStdoutBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(b)
	q := fmt.Sprintf("INSERT INTO kv(key,value,updated_at) VALUES(%s,%s,%s) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at;\n", quote(key), quote(encoded), quote(time.Now().UTC().Format(time.RFC3339Nano)))
	_, err = s.sqlite(ctx, q)
	return err
}

func (s *Store) Get(ctx context.Context, key string, out any) error {
	if err := validContext(ctx); err != nil {
		return err
	}
	var b []byte
	var err error
	if s.sqlitePath == "" {
		b, err = s.getFallback(key)
	} else {
		var result string
		result, err = s.sqlite(ctx, "SELECT value FROM kv WHERE key="+quote(key)+";\n")
		if err == nil {
			result = strings.TrimSpace(result)
			if result == "" {
				return os.ErrNotExist
			}
			b, err = base64.StdEncoding.DecodeString(result)
		}
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *Store) SaveSnapshot(ctx context.Context, snapshot domain.Snapshot) error {
	return s.Put(ctx, "snapshot/latest", snapshot)
}

func (s *Store) LoadSnapshot(ctx context.Context) (domain.Snapshot, error) {
	var snapshot domain.Snapshot
	err := s.Get(ctx, "snapshot/latest", &snapshot)
	return snapshot, err
}

func (s *Store) SavePlan(ctx context.Context, plan domain.ActionPlan) error {
	return s.Put(ctx, "plan/"+plan.ID, plan)
}

func (s *Store) LoadPlan(ctx context.Context, id string) (domain.ActionPlan, error) {
	var plan domain.ActionPlan
	err := s.Get(ctx, "plan/"+id, &plan)
	return plan, err
}

func (s *Store) sqlite(ctx context.Context, script string) (string, error) {
	if err := validContext(ctx); err != nil {
		return "", err
	}
	// Do not validate a path and then ask sqlite3 to reopen it: the shared
	// collector account can legitimately replace cache state, which otherwise
	// creates a symlink-swap window when a privileged caller uses the store.
	// Pin the singly-linked regular file first and let sqlite3 reopen that exact
	// inode through its inherited descriptor. MEMORY journaling avoids unsafe
	// sidecar path construction beside /proc/self/fd/3. This database is cache
	// and inventory state, never the authoritative mutation audit.
	file, err := openSharedStateFile(s.path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	cmd := exec.CommandContext(ctx, s.sqlitePath, "-batch", "-noheader", "/proc/self/fd/3")
	cmd.Env = []string{"PATH=" + execx.TrustedPath, "LC_ALL=C", "LANG=C", "TZ=UTC"}
	cmd.ExtraFiles = []*os.File{file}
	cmd.Stdin = strings.NewReader("PRAGMA journal_mode=MEMORY;\nPRAGMA synchronous=FULL;\n" + script)
	stdout := execx.NewBoundedBuffer(s.maxStdoutBytes)
	stderr := execx.NewBoundedBuffer(s.maxStderrBytes)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	// exec.CommandContext may report only the signal used to stop sqlite3.
	// Preserve the caller's cause so SIGINT/deadline handling can distinguish
	// cancellation from a database failure at the CLI boundary.
	if contextErr := ctx.Err(); contextErr != nil {
		runErr = errors.Join(runErr, contextErr)
	}
	runErr = errors.Join(runErr, stdout.LimitError("sqlite stdout"), stderr.LimitError("sqlite stderr"))
	if runErr != nil {
		return stdout.String(), fmt.Errorf("sqlite3: %w: %s", runErr, boundedSQLiteDetail(stderr.String()))
	}
	output := stdout.String()
	mode, remainder, found := strings.Cut(output, "\n")
	if !found || strings.TrimSpace(mode) != "memory" {
		return "", fmt.Errorf("sqlite3 did not enter descriptor-safe MEMORY journal mode: %q", strings.TrimSpace(mode))
	}
	return remainder, nil
}

func boundedSQLiteDetail(value string) string {
	const maximum = 1024
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "..."
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func validContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil state-store context")
	}
	return ctx.Err()
}

func (s *Store) fallbackPath() string { return s.path + ".json" }

func (s *Store) readFallback() (fallbackState, error) {
	state := fallbackState{Schema: schemaVersion, Values: map[string]json.RawMessage{}}
	fd, err := syscall.Open(s.fallbackPath(), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	file := os.NewFile(uintptr(fd), s.fallbackPath())
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return state, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		return state, errors.New("fallback state must be a singly linked regular file")
	}
	if stat.Size > maxFallbackStateBytes {
		return state, fmt.Errorf("fallback state exceeds %d bytes", maxFallbackStateBytes)
	}
	b, err := io.ReadAll(io.LimitReader(file, maxFallbackStateBytes+1))
	if err != nil {
		return state, err
	}
	if len(b) > maxFallbackStateBytes {
		return state, fmt.Errorf("fallback state exceeds %d bytes", maxFallbackStateBytes)
	}
	var decoded fallbackState
	if err := json.Unmarshal(b, &decoded); err != nil {
		return state, fmt.Errorf("state is corrupt: %w", err)
	}
	state = decoded
	if state.Schema != schemaVersion {
		return state, fmt.Errorf("unsupported state schema %d", state.Schema)
	}
	if state.Values == nil {
		return state, errors.New("state is corrupt: missing values object")
	}
	return state, nil
}

func (s *Store) putFallback(ctx context.Context, key string, value []byte) error {
	lock, err := lockfile.Acquire(ctx, s.fallbackPath()+".lock", true)
	if err != nil {
		return err
	}
	defer lock.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readFallback()
	if err != nil {
		return err
	}
	state.Values[key] = append(json.RawMessage(nil), value...)
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.writeFallback(state)
}

func ensureSharedStateFile(path string) error {
	file, err := openSharedStateFile(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func openSharedStateFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o660)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fail(fmt.Errorf("stat state database: %w", err))
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		return fail(errors.New("state database must be a singly linked regular file"))
	}
	if stat.Mode&0o660 != 0o660 {
		if err := syscall.Fchmod(fd, 0o660); err != nil {
			return fail(fmt.Errorf("state database must be group-writable for both collectors: %w", err))
		}
	}
	return file, nil
}

func (s *Store) getFallback(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readFallback()
	if err != nil {
		return nil, err
	}
	b, ok := state.Values[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func (s *Store) writeFallback(state fallbackState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxFallbackStateBytes {
		return fmt.Errorf("fallback state exceeds %d bytes", maxFallbackStateBytes)
	}
	dir := filepath.Dir(s.fallbackPath())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".zpoolpilot-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o660); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.fallbackPath())
}
