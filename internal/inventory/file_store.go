// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

const (
	BayStoreAPIVersion = "github.com/bouldinnathan/zpoolpilot/bays/v1"
	maxBayStoreBytes   = 1 << 20
	bayStoreTrustRoot  = "/etc/zpoolpilot"
)

// ErrBayStoreMissing means the authenticated parent directory exists but the
// final bay mapping file does not. Callers may treat only this condition as an
// optional empty store; a missing or untrusted parent remains a visible error.
var ErrBayStoreMissing = errors.New("authoritative bay mapping store is missing")

type bayFile struct {
	APIVersion string                `json:"api_version"`
	Mappings   map[string]domain.Bay `json:"mappings"`
}

// BayFileStore is the authoritative manual-mapping store. Production stores
// are confined beneath /etc/zpoolpilot and accept only root-owned, singly linked
// regular files reached through a root-owned, non-writable directory chain.
// Monitoring accounts may read the file but can never create or update it.
type BayFileStore struct {
	path              string
	trustRoot         string
	expectedOwner     uint32
	requireRootWriter bool
}

func OpenBayFile(path string) *BayFileStore {
	return &BayFileStore{path: path, trustRoot: bayStoreTrustRoot, expectedOwner: 0, requireRootWriter: true}
}

func (store *BayFileStore) LoadBays(ctx context.Context) (map[string]domain.Bay, error) {
	if ctx == nil {
		return nil, errors.New("nil bay-store context")
	}
	if store == nil {
		return nil, errors.New("nil bay-file store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := store.read()
	if err != nil {
		return nil, err
	}
	result := make(map[string]domain.Bay, len(state.Mappings))
	for driveID, bay := range state.Mappings {
		result[driveID] = bay
	}
	return result, nil
}

func (store *BayFileStore) SaveBay(ctx context.Context, driveID string, bay domain.Bay) error {
	if ctx == nil {
		return errors.New("nil bay-store context")
	}
	if store == nil {
		return errors.New("nil bay-file store")
	}
	if store.requireRootWriter && os.Geteuid() != 0 {
		return errors.New("authoritative bay mappings may be written only by root")
	}
	if !stableMappingDriveID(driveID) {
		return errors.New("unsafe drive identity for bay mapping")
	}
	if err := validateStoredBay(bay); err != nil {
		return err
	}
	lock, err := store.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.close()

	state, err := store.read()
	if errors.Is(err, ErrBayStoreMissing) {
		state = bayFile{APIVersion: BayStoreAPIVersion, Mappings: make(map[string]domain.Bay)}
	} else if err != nil {
		return err
	}
	if state.Mappings == nil {
		state.Mappings = make(map[string]domain.Bay)
	}
	for otherID, otherBay := range state.Mappings {
		if otherID != driveID && sameBay(otherBay, bay) {
			return fmt.Errorf("bay %q is already mapped to drive %s", bay.Label, otherID)
		}
	}
	state.Mappings[driveID] = bay
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.write(state)
}

// DeleteBay removes exactly one stored mapping. The expected value is checked
// while holding the writer lock so a mapping cannot change between the
// operator's confirmation and deletion.
func (store *BayFileStore) DeleteBay(ctx context.Context, driveID string, expected domain.Bay) error {
	if ctx == nil {
		return errors.New("nil bay-store context")
	}
	if store == nil {
		return errors.New("nil bay-file store")
	}
	if store.requireRootWriter && os.Geteuid() != 0 {
		return errors.New("authoritative bay mappings may be written only by root")
	}
	if !stableMappingDriveID(driveID) {
		return errors.New("unsafe drive identity for bay mapping deletion")
	}
	if err := validateStoredBay(expected); err != nil {
		return fmt.Errorf("invalid expected bay mapping: %w", err)
	}
	lock, err := store.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.close()

	state, err := store.read()
	if err != nil {
		return err
	}
	current, present := state.Mappings[driveID]
	if !present {
		return fmt.Errorf("bay mapping for exact drive ID %q no longer exists", driveID)
	}
	if !equalStoredBay(current, expected) {
		return fmt.Errorf("bay mapping for exact drive ID %q changed after confirmation; deletion was refused", driveID)
	}
	delete(state.Mappings, driveID)
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.write(state)
}

func equalStoredBay(left, right domain.Bay) bool {
	return left.Enclosure == right.Enclosure && left.EnclosureIdentity == right.EnclosureIdentity &&
		left.Label == right.Label && left.Slot == right.Slot && left.SlotKnown == right.SlotKnown &&
		left.Source == right.Source && left.Confidence == right.Confidence && left.VerifiedAt.Equal(right.VerifiedAt)
}

func (store *BayFileStore) read() (bayFile, error) {
	empty := bayFile{APIVersion: BayStoreAPIVersion, Mappings: make(map[string]domain.Bay)}
	if err := store.validatePath(); err != nil {
		return empty, err
	}
	fd, err := syscall.Open(store.path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, fmt.Errorf("%w: %s", ErrBayStoreMissing, store.path)
		}
		return empty, fmt.Errorf("open bay mapping store: %w", err)
	}
	file := os.NewFile(uintptr(fd), store.path)
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return empty, fmt.Errorf("stat bay mapping store: %w", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 || stat.Uid != store.expectedOwner {
		return empty, fmt.Errorf("bay mapping store must be a singly linked regular file owned by uid %d", store.expectedOwner)
	}
	if stat.Mode&0o022 != 0 {
		return empty, errors.New("bay mapping store must not be writable by group or other")
	}
	if stat.Size > maxBayStoreBytes {
		return empty, fmt.Errorf("bay mapping store exceeds %d bytes", maxBayStoreBytes)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxBayStoreBytes+1))
	decoder.DisallowUnknownFields()
	var state bayFile
	if err := decoder.Decode(&state); err != nil {
		return empty, fmt.Errorf("decode bay mapping store: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return empty, fmt.Errorf("decode bay mapping store: %w", err)
	}
	if state.APIVersion != BayStoreAPIVersion || state.Mappings == nil {
		return empty, fmt.Errorf("unsupported or incomplete bay mapping schema %q", state.APIVersion)
	}
	validated := make(map[string]domain.Bay, len(state.Mappings))
	for driveID, bay := range state.Mappings {
		if !stableMappingDriveID(driveID) {
			return empty, errors.New("bay mapping store contains a volatile or unsafe drive identity")
		}
		if err := validateStoredBay(bay); err != nil {
			return empty, fmt.Errorf("bay mapping for %q: %w", driveID, err)
		}
		for otherID, otherBay := range validated {
			if sameBay(otherBay, bay) {
				return empty, fmt.Errorf("bay mappings for %q and %q identify the same physical bay", otherID, driveID)
			}
		}
		validated[driveID] = bay
	}
	return state, nil
}

func stableMappingDriveID(value string) bool {
	if !safeMappingText(value) {
		return false
	}
	if strings.HasPrefix(value, "wwn:") {
		canonical, ok := CanonicalDriveWWN(strings.TrimPrefix(value, "wwn:"))
		return ok && "wwn:"+canonical == value
	}
	if !strings.HasPrefix(value, "serial:") || value != strings.ToLower(value) {
		return false
	}
	// Serial fallback IDs are generated as serial|model|capacity. Requiring
	// this exact form prevents a hand-written prefix from masquerading as a
	// discovery identity. Hardware serials containing '|' are intentionally
	// refused because the representation would be ambiguous.
	parts := strings.Split(strings.TrimPrefix(value, "serial:"), "|")
	if len(parts) != 3 {
		return false
	}
	if _, ok := CanonicalSESSerial(parts[0]); !ok {
		return false
	}
	capacity, err := strconv.ParseUint(parts[2], 10, 64)
	return err == nil && capacity > 0
}

func validateStoredBay(bay domain.Bay) error {
	if bay.Source != "manual" {
		return errors.New("stored bay mapping source must be manual")
	}
	if !safeMappingText(bay.Label) {
		return errors.New("stored bay mapping requires a safe nonempty label")
	}
	if bay.Enclosure != "" && !safeMappingText(bay.Enclosure) {
		return errors.New("stored bay enclosure contains unsafe text")
	}
	if bay.SlotKnown && bay.Slot < 0 {
		return errors.New("stored bay slot cannot be negative")
	}
	if !bay.SlotKnown && bay.Slot != 0 {
		return errors.New("stored bay slot must be zero when slot_known is false")
	}
	switch bay.Confidence {
	case BayConfidenceInferred:
		if !bay.VerifiedAt.IsZero() || bay.EnclosureIdentity != "" {
			return errors.New("inferred mapping must not claim verification metadata")
		}
	case BayConfidenceVerified:
		if bay.VerifiedAt.IsZero() || !bay.SlotKnown || bay.Slot < 0 || !ValidSESDevicePath(bay.Enclosure) || !ValidSESEnclosureIdentity(bay.EnclosureIdentity) {
			return errors.New("verified mapping requires timestamp, known nonnegative slot, canonical SES path, and stable enclosure identity")
		}
	default:
		return errors.New("stored bay confidence must be inferred or verified")
	}
	return nil
}

func (store *BayFileStore) validatePath() error {
	if store == nil || !filepath.IsAbs(store.path) || filepath.Clean(store.path) != store.path {
		return errors.New("bay mapping path must be absolute and normalized")
	}
	root := filepath.Clean(store.trustRoot)
	relative, err := filepath.Rel(root, store.path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("bay mapping path must remain beneath %s", root)
	}
	directory := filepath.Dir(store.path)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve bay mapping directory: %w", err)
	}
	if resolved != directory {
		return errors.New("bay mapping directory path must not contain symbolic links")
	}
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect bay mapping directory %q: %w", current, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || stat.Uid != store.expectedOwner || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("bay mapping directory %q must be owned by uid %d and not writable by group/other", current, store.expectedOwner)
		}
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || !pathAtOrBelow(parent, root) {
			return fmt.Errorf("bay mapping directory escapes trusted root %s", root)
		}
	}
	return nil
}

func pathAtOrBelow(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (relative == "." || relative != ".." && !filepath.IsAbs(relative) &&
		!(len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator)))
}

type bayFileLock struct{ file *os.File }

func (store *BayFileStore) lock(ctx context.Context) (*bayFileLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.validatePath(); err != nil {
		return nil, err
	}
	path := store.path + ".lock"
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open bay mapping lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*bayFileLock, error) {
		_ = file.Close()
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fail(fmt.Errorf("stat bay mapping lock: %w", err))
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 || stat.Uid != store.expectedOwner || stat.Mode&0o022 != 0 {
		return fail(errors.New("bay mapping lock must be a protected singly linked regular file"))
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			return &bayFileLock{file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return fail(fmt.Errorf("lock bay mapping store: %w", err))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (lock *bayFileLock) close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	fd := int(lock.file.Fd())
	err := syscall.Flock(fd, syscall.LOCK_UN)
	err = errors.Join(err, lock.file.Close())
	lock.file = nil
	return err
}

func (store *BayFileStore) write(state bayFile) error {
	state.APIVersion = BayStoreAPIVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bay mapping store: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxBayStoreBytes {
		return fmt.Errorf("bay mapping store exceeds %d bytes", maxBayStoreBytes)
	}
	directory := filepath.Dir(store.path)
	temporary, err := os.CreateTemp(directory, ".bays-*")
	if err != nil {
		return fmt.Errorf("create bay mapping temporary file: %w", err)
	}
	name := temporary.Name()
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("set bay mapping permissions: %w", err)
	}
	info, err := temporary.Stat()
	if err != nil {
		return fmt.Errorf("stat bay mapping temporary file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != store.expectedOwner || !info.Mode().IsRegular() {
		return errors.New("bay mapping temporary file has an untrusted owner or type")
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write bay mapping store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync bay mapping store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close bay mapping store: %w", err)
	}
	if err := os.Rename(name, store.path); err != nil {
		return fmt.Errorf("install bay mapping store: %w", err)
	}
	remove = false
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open bay mapping directory for sync: %w", err)
	}
	err = directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	return errors.Join(err, closeErr)
}
