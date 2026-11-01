// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

type canonicalTopology struct {
	Host  string          `json:"host"`
	Pools []canonicalPool `json:"pools"`
}

type canonicalPool struct {
	Name         string          `json:"name"`
	GUID         string          `json:"guid"`
	BootCritical bool            `json:"boot_critical"`
	VDevs        []canonicalVDev `json:"vdevs"`
}

type canonicalVDev struct {
	Name          string          `json:"name"`
	GUID          string          `json:"guid"`
	Type          string          `json:"type"`
	Class         string          `json:"class"`
	StableDriveID string          `json:"stable_drive_id"`
	SizeBytes     uint64          `json:"size_bytes"`
	Children      []canonicalVDev `json:"children"`
}

type canonicalSnapshot struct {
	Topology canonicalTopology `json:"topology"`
	Pools    []poolState       `json:"pool_state"`
	Drives   []driveState      `json:"drives"`
}

type poolState struct {
	GUID  string      `json:"guid"`
	State string      `json:"state"`
	VDevs []vdevState `json:"vdevs"`
}

type vdevState struct {
	GUID     string      `json:"guid"`
	Name     string      `json:"name"`
	State    string      `json:"state"`
	Children []vdevState `json:"children"`
}

type driveState struct {
	ID                  string `json:"id"`
	WWN                 string `json:"wwn"`
	Serial              string `json:"serial"`
	ByID                string `json:"by_id"`
	CapacityBytes       uint64 `json:"capacity_bytes"`
	LogicalSectorBytes  uint64 `json:"logical_sector_bytes"`
	PhysicalSectorBytes uint64 `json:"physical_sector_bytes"`
	InUse               bool   `json:"in_use"`
	Use                 string `json:"use"`
}

// TopologyFingerprint hashes only stable host, pool, and vdev topology. It is
// unaffected by scan progress, performance counters, and collection time.
func TopologyFingerprint(snapshot domain.Snapshot) string {
	value := makeCanonicalTopology(snapshot)
	return hashJSON(value)
}

// SnapshotFingerprint also covers drive identities, geometry, current use,
// and pool state: all inputs which can invalidate a destructive preview.
func SnapshotFingerprint(snapshot domain.Snapshot) string {
	value := canonicalSnapshot{Topology: makeCanonicalTopology(snapshot)}
	for _, pool := range snapshot.Pools {
		state := poolState{GUID: pool.GUID, State: pool.State}
		for _, vdev := range pool.VDevs {
			state.VDevs = append(state.VDevs, makeVDevState(vdev))
		}
		sort.Slice(state.VDevs, func(i, j int) bool { return state.VDevs[i].GUID < state.VDevs[j].GUID })
		value.Pools = append(value.Pools, state)
	}
	sort.Slice(value.Pools, func(i, j int) bool { return value.Pools[i].GUID < value.Pools[j].GUID })
	for _, drive := range snapshot.Drives {
		value.Drives = append(value.Drives, driveState{
			ID: drive.ID, WWN: drive.WWN, Serial: drive.Serial, ByID: drive.ByID,
			CapacityBytes: drive.CapacityBytes, LogicalSectorBytes: drive.LogicalSectorBytes,
			PhysicalSectorBytes: drive.PhysicalSectorBytes, InUse: drive.InUse, Use: drive.Use,
		})
	}
	sort.Slice(value.Drives, func(i, j int) bool {
		if value.Drives[i].ID == value.Drives[j].ID {
			return value.Drives[i].ByID < value.Drives[j].ByID
		}
		return value.Drives[i].ID < value.Drives[j].ID
	})
	return hashJSON(value)
}

func makeVDevState(vdev domain.VDev) vdevState {
	result := vdevState{GUID: vdev.GUID, Name: vdev.Name, State: vdev.State}
	for _, child := range vdev.Children {
		result.Children = append(result.Children, makeVDevState(child))
	}
	sort.Slice(result.Children, func(i, j int) bool {
		if result.Children[i].GUID == result.Children[j].GUID {
			return result.Children[i].Name < result.Children[j].Name
		}
		return result.Children[i].GUID < result.Children[j].GUID
	})
	return result
}

// DriveIdentityFingerprint excludes transient devnodes and health data.
func DriveIdentityFingerprint(drive domain.Drive) string {
	return hashJSON(driveState{
		ID: drive.ID, WWN: drive.WWN, Serial: drive.Serial, ByID: drive.ByID,
		CapacityBytes: drive.CapacityBytes, LogicalSectorBytes: drive.LogicalSectorBytes,
		PhysicalSectorBytes: drive.PhysicalSectorBytes,
	})
}

// Fingerprint is the immutable content fingerprint used in confirmation and
// audit records. Confirmation is cleared to avoid a circular hash dependency.
func Fingerprint(plan domain.ActionPlan) string {
	plan.Confirmation = ""
	return hashJSON(plan)
}

func makeCanonicalTopology(snapshot domain.Snapshot) canonicalTopology {
	result := canonicalTopology{Host: snapshot.Host.MachineID}
	for _, pool := range snapshot.Pools {
		item := canonicalPool{Name: pool.Name, GUID: pool.GUID, BootCritical: pool.BootCritical}
		for _, vdev := range pool.VDevs {
			item.VDevs = append(item.VDevs, makeCanonicalVDev(vdev))
		}
		sort.Slice(item.VDevs, func(i, j int) bool { return vdevKey(item.VDevs[i]) < vdevKey(item.VDevs[j]) })
		result.Pools = append(result.Pools, item)
	}
	sort.Slice(result.Pools, func(i, j int) bool {
		if result.Pools[i].GUID == result.Pools[j].GUID {
			return result.Pools[i].Name < result.Pools[j].Name
		}
		return result.Pools[i].GUID < result.Pools[j].GUID
	})
	return result
}

func makeCanonicalVDev(vdev domain.VDev) canonicalVDev {
	result := canonicalVDev{
		Name: vdev.Name, GUID: vdev.GUID, Type: vdev.Type, Class: vdev.Class,
		StableDriveID: vdev.StableDriveID, SizeBytes: vdev.SizeBytes,
	}
	for _, child := range vdev.Children {
		result.Children = append(result.Children, makeCanonicalVDev(child))
	}
	sort.Slice(result.Children, func(i, j int) bool { return vdevKey(result.Children[i]) < vdevKey(result.Children[j]) })
	return result
}

func vdevKey(vdev canonicalVDev) string {
	return vdev.GUID + "\x00" + vdev.Name + "\x00" + vdev.StableDriveID
}

func hashJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err) // all inputs are fixed JSON-safe structures
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func sameStringSet(left, right []string) bool {
	left = slices.Clone(left)
	right = slices.Clone(right)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}
