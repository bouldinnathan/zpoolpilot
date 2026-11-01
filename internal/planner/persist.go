// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

const maxPlanFileBytes = 1 << 20

// SavePlan atomically writes a validated plan with owner-only permissions.
func SavePlan(path string, plan domain.ActionPlan) error {
	return SavePlanAt(path, plan, time.Now().UTC())
}

func SavePlanAt(path string, plan domain.ActionPlan, now time.Time) error {
	if path == "" {
		return fmt.Errorf("%w: empty plan path", ErrInvalidPlan)
	}
	if err := ValidatePlan(plan, now); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".zpoolpilot-plan-*")
	if err != nil {
		return fmt.Errorf("create temporary plan: %w", err)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary plan: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(plan); err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync plan: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close plan: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("install plan: %w", err)
	}
	removeTemporary = false
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

// LoadPlan decodes one strictly bounded JSON plan and rejects expired or
// structurally modified content.
func LoadPlan(path string) (domain.ActionPlan, error) {
	return LoadPlanAt(path, time.Now().UTC())
}

func LoadPlanAt(path string, now time.Time) (domain.ActionPlan, error) {
	// Open without waiting for a writer when an untrusted path is a FIFO;
	// the descriptor type is checked before any JSON is read.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return domain.ActionPlan{}, fmt.Errorf("open plan: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return domain.ActionPlan{}, fmt.Errorf("stat plan: %w", err)
	} else if !info.Mode().IsRegular() {
		return domain.ActionPlan{}, fmt.Errorf("%w: plan is not a regular file", ErrInvalidPlan)
	} else if info.Mode().Perm()&0o022 != 0 {
		return domain.ActionPlan{}, fmt.Errorf("%w: plan is writable by group or other", ErrInvalidPlan)
	} else if info.Size() > maxPlanFileBytes {
		return domain.ActionPlan{}, fmt.Errorf("%w: plan exceeds %d bytes", ErrInvalidPlan, maxPlanFileBytes)
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxPlanFileBytes+1))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var plan domain.ActionPlan
	if err := decoder.Decode(&plan); err != nil {
		return domain.ActionPlan{}, fmt.Errorf("decode plan: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return domain.ActionPlan{}, fmt.Errorf("decode plan: %w", err)
	}
	if err := ValidatePlan(plan, now); err != nil {
		return domain.ActionPlan{}, err
	}
	return plan, nil
}
