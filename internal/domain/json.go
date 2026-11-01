// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"encoding/json"
	"time"
)

// Required collection fields keep their schema types even when a collector
// found no records or failed before it could initialize them. Work on copies
// so serialization does not mutate caller-owned snapshots.
func (value Snapshot) MarshalJSON() ([]byte, error) {
	type alias Snapshot
	value.Pools = nonnilSlice(value.Pools)
	value.Datasets = nonnilSlice(value.Datasets)
	value.Drives = nonnilSlice(value.Drives)
	value.Findings = nonnilSlice(value.Findings)
	value.Errors = nonnilSlice(value.Errors)
	return json.Marshal(alias(value))
}

func (value Pool) MarshalJSON() ([]byte, error) {
	type alias Pool
	value.VDevs = nonnilSlice(value.VDevs)
	return json.Marshal(alias(value))
}

func (value Dataset) MarshalJSON() ([]byte, error) {
	type alias Dataset
	if value.Properties == nil {
		value.Properties = map[string]string{}
	}
	if value.PropertySources == nil {
		value.PropertySources = map[string]string{}
	}
	return json.Marshal(alias(value))
}

func (value CommandTrace) MarshalJSON() ([]byte, error) {
	type alias CommandTrace
	value.Args = nonnilSlice(value.Args)
	return json.Marshal(alias(value))
}

func nonnilSlice[T any](value []T) []T {
	if value == nil {
		return []T{}
	}
	return value
}

// time.Time is a struct, so encoding/json's omitempty rule does not omit its
// zero value on Go 1.23. These wire shims keep unavailable timestamps absent
// without changing the convenient value-type API used by discovery and safety
// checks. Decoding continues to use the ordinary struct fields.

func (value Scan) MarshalJSON() ([]byte, error) {
	type alias Scan
	return json.Marshal(struct {
		alias
		EndedAt *time.Time `json:"ended_at,omitempty"`
	}{alias: alias(value), EndedAt: nonzeroTime(value.EndedAt)})
}

func (value TrimStats) MarshalJSON() ([]byte, error) {
	type alias TrimStats
	return json.Marshal(struct {
		alias
		ActionTime *time.Time `json:"action_time,omitempty"`
	}{alias: alias(value), ActionTime: nonzeroTime(value.ActionTime)})
}

func (value Bay) MarshalJSON() ([]byte, error) {
	type alias Bay
	return json.Marshal(struct {
		alias
		VerifiedAt *time.Time `json:"verified_at,omitempty"`
	}{alias: alias(value), VerifiedAt: nonzeroTime(value.VerifiedAt)})
}

func (value DriveHealth) MarshalJSON() ([]byte, error) {
	type alias DriveHealth
	return json.Marshal(struct {
		alias
		FreshAt *time.Time `json:"fresh_at,omitempty"`
	}{alias: alias(value), FreshAt: nonzeroTime(value.FreshAt)})
}

func nonzeroTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}
