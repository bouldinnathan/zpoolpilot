// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

type BayStore interface {
	LoadBays(context.Context) (map[string]domain.Bay, error)
	SaveBay(context.Context, string, domain.Bay) error
}

const (
	BayConfidenceInferred = "inferred"
	BayConfidenceVerified = "verified"
	bayConfidenceConflict = "conflict"
	bayConflictIDPrefix   = "bay-conflict-"
)

// MergeMappedBays overlays manual mappings on automatic discovery. Inferred
// manual mappings are retained so an operator can review and subsequently
// promote the exact stored values with inventory verify.
func MergeMappedBays(ctx context.Context, snapshot *domain.Snapshot, store BayStore) error {
	if snapshot == nil {
		return nil
	}
	// A cached snapshot is not authority for a manual mapping. Clear any
	// previously overlaid value before consulting the separately authenticated
	// bay store so removing or invalidating that store cannot leave stale
	// verified metadata behind.
	for index := range snapshot.Drives {
		if snapshot.Drives[index].Bay.Source == "manual" {
			snapshot.Drives[index].Bay = domain.Bay{Confidence: "unknown"}
		}
	}
	clearBayConflictState(snapshot)
	if store != nil {
		mappings, err := store.LoadBays(ctx)
		if err != nil && !errors.Is(err, ErrBayStoreMissing) {
			detectConflicts(snapshot)
			return err
		}
		for i := range snapshot.Drives {
			bay, present := mappings[snapshot.Drives[i].ID]
			if present && bay.Source == "manual" &&
				(bay.Confidence == BayConfidenceInferred || bay.Confidence == BayConfidenceVerified) {
				snapshot.Drives[i].Bay = bay
			}
		}
	}
	detectConflicts(snapshot)
	return nil
}

func MapBay(ctx context.Context, store BayStore, snapshot domain.Snapshot, driveID string, bay domain.Bay) error {
	if ctx == nil {
		return errors.New("nil inventory context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil {
		return errors.New("inventory store is required")
	}
	drive, ok := FindDrive(snapshot, driveID)
	if !ok {
		return fmt.Errorf("drive %q is not present in the current snapshot", driveID)
	}
	if drive.ID == "" {
		return errors.New("drive has no stable identity")
	}
	if err := ValidateStableDriveIdentity(snapshot, drive); err != nil {
		return err
	}
	if !safeMappingText(drive.ID) {
		return errors.New("drive identity contains unsafe text")
	}
	bay.Label = strings.TrimSpace(bay.Label)
	bay.Enclosure = strings.TrimSpace(bay.Enclosure)
	bay.EnclosureIdentity = strings.TrimSpace(bay.EnclosureIdentity)
	if bay.Label == "" {
		return errors.New("bay label is required")
	}
	if !safeMappingText(bay.Label) || (bay.Enclosure != "" && !safeMappingText(bay.Enclosure)) ||
		(bay.EnclosureIdentity != "" && !safeMappingText(bay.EnclosureIdentity)) {
		return errors.New("bay mapping contains unsafe text")
	}
	if bay.SlotKnown && bay.Slot < 0 {
		return errors.New("bay slot cannot be negative")
	}
	if !bay.SlotKnown {
		bay.Slot = 0
	}
	if bay.Confidence != BayConfidenceInferred && bay.Confidence != BayConfidenceVerified {
		return fmt.Errorf("bay confidence must be %q or %q", BayConfidenceInferred, BayConfidenceVerified)
	}
	bay.Source = "manual"
	if bay.Confidence == BayConfidenceVerified {
		if !bay.SlotKnown {
			return errors.New("verified bay mapping requires an explicitly known numeric slot")
		}
		if bay.EnclosureIdentity == "" {
			return errors.New("verified bay mapping requires a stable enclosure identity")
		}
		bay.VerifiedAt = time.Now().UTC()
	} else {
		bay.EnclosureIdentity = ""
		bay.VerifiedAt = time.Time{}
	}
	for _, other := range snapshot.Drives {
		if other.ID == drive.ID {
			continue
		}
		if sameBay(other.Bay, bay) {
			return fmt.Errorf("bay %q is already occupied by drive %s", bay.Label, displayID(other))
		}
	}
	return store.SaveBay(ctx, drive.ID, bay)
}

// ValidateStableDriveIdentity rejects volatile devnode-derived identities and
// ambiguous firmware identifiers before a manual physical mapping can become
// authority. WWN is preferred; serial is accepted only as a unique fallback.
func ValidateStableDriveIdentity(snapshot domain.Snapshot, drive domain.Drive) error {
	if drive.ID == "" {
		return errors.New("drive has no stable identity")
	}
	idCount := 0
	for _, candidate := range snapshot.Drives {
		if candidate.ID == drive.ID {
			idCount++
		}
	}
	if idCount != 1 {
		return fmt.Errorf("drive %q stable identity resolves to %d present devices", drive.ID, idCount)
	}
	if strings.TrimSpace(drive.WWN) != "" {
		canonical, reliable := CanonicalDriveWWN(drive.WWN)
		if !reliable || "wwn:"+canonical != drive.ID {
			return fmt.Errorf("drive %q has a noncanonical or placeholder WWN identity", drive.ID)
		}
		count := 0
		for _, candidate := range snapshot.Drives {
			if strings.EqualFold(strings.TrimSpace(candidate.WWN), strings.TrimSpace(drive.WWN)) {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("drive %q WWN resolves to %d present devices", drive.ID, count)
		}
		return nil
	}
	if strings.TrimSpace(drive.Serial) == "" {
		return fmt.Errorf("drive %q has only a volatile path identity; a WWN or unique serial is required", drive.ID)
	}
	if _, reliable := CanonicalSESSerial(drive.Serial); !reliable {
		return fmt.Errorf("drive %q has a placeholder or unreliable hardware serial", drive.ID)
	}
	if drive.CapacityBytes == 0 || strings.Contains(drive.Serial, "|") || strings.Contains(drive.Model, "|") {
		return fmt.Errorf("drive %q cannot form an unambiguous serial fallback identity", drive.ID)
	}
	expectedID := "serial:" + strings.ToLower(strings.Join([]string{
		drive.Serial, drive.Model, strconv.FormatUint(drive.CapacityBytes, 10),
	}, "|"))
	if drive.ID != expectedID {
		return fmt.Errorf("drive %q does not match its canonical serial fallback identity %q", drive.ID, expectedID)
	}
	count := 0
	for _, candidate := range snapshot.Drives {
		if strings.EqualFold(strings.TrimSpace(candidate.Serial), strings.TrimSpace(drive.Serial)) {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("drive %q serial resolves to %d present devices", drive.ID, count)
	}
	return nil
}

func safeMappingText(value string) bool {
	if value == "" || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || (character >= 0x80 && character <= 0x9f) {
			return false
		}
	}
	return true
}

func DetectConflicts(snapshot *domain.Snapshot) {
	if snapshot == nil {
		return
	}
	clearBayConflictState(snapshot)
	detectConflicts(snapshot)
}

func clearBayConflictState(snapshot *domain.Snapshot) {
	for i := range snapshot.Drives {
		if strings.EqualFold(strings.TrimSpace(snapshot.Drives[i].Bay.Confidence), bayConfidenceConflict) {
			if snapshot.Drives[i].Bay.Source == "by-path" {
				snapshot.Drives[i].Bay.Confidence = BayConfidenceInferred
			} else {
				snapshot.Drives[i].Bay.Confidence = ""
			}
		}
	}
	findings := snapshot.Findings[:0]
	for _, finding := range snapshot.Findings {
		if strings.HasPrefix(finding.ID, bayConflictIDPrefix) {
			continue
		}
		findings = append(findings, finding)
	}
	snapshot.Findings = findings
}

func detectConflicts(snapshot *domain.Snapshot) {
	existing := make(map[string]bool, len(snapshot.Findings))
	for _, finding := range snapshot.Findings {
		existing[finding.ID] = true
	}
	for i := range snapshot.Drives {
		for j := i + 1; j < len(snapshot.Drives); j++ {
			if snapshot.Drives[i].Bay.Label != "" && sameBay(snapshot.Drives[i].Bay, snapshot.Drives[j].Bay) {
				snapshot.Drives[i].Bay.Confidence = bayConfidenceConflict
				snapshot.Drives[j].Bay.Confidence = bayConfidenceConflict
				id := bayConflictIDPrefix + snapshot.Drives[i].ID + "-" + snapshot.Drives[j].ID
				if !existing[id] {
					snapshot.Findings = append(snapshot.Findings, domain.Finding{ID: id, Severity: domain.SeverityCritical, Scope: "inventory", Title: "Two present drives map to the same physical bay", Evidence: fmt.Sprintf("%s and %s both map to %s", displayID(snapshot.Drives[i]), displayID(snapshot.Drives[j]), snapshot.Drives[i].Bay.Label), Remediation: "Verify enclosure data and manually remap the bay before planning a storage change."})
					existing[id] = true
				}
			}
		}
	}
}

func FindDrive(snapshot domain.Snapshot, query string) (domain.Drive, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return domain.Drive{}, false
	}
	var matches []domain.Drive
	for _, drive := range snapshot.Drives {
		if query == drive.ID || query == drive.WWN || query == drive.Serial || query == drive.ByID || query == drive.DevNode {
			matches = append(matches, drive)
		}
	}
	return uniqueDrive(matches)
}

func FindPool(snapshot domain.Snapshot, query string) (domain.Pool, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return domain.Pool{}, false
	}
	var match domain.Pool
	found := false
	for _, pool := range snapshot.Pools {
		if pool.Name == query || pool.GUID == query {
			if found {
				return domain.Pool{}, false
			}
			match, found = pool, true
		}
	}
	return match, found
}

func FindVDev(pool domain.Pool, query string) (domain.VDev, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return domain.VDev{}, false
	}
	var matches []domain.VDev
	var walk func([]domain.VDev)
	walk = func(vdevs []domain.VDev) {
		for _, v := range vdevs {
			if v.Name == query || v.GUID == query || v.Path == query || v.StableDriveID == query {
				matches = append(matches, v)
			}
			walk(v.Children)
		}
	}
	walk(pool.VDevs)
	if len(matches) == 1 {
		return matches[0], true
	}
	return domain.VDev{}, false
}

func DriveIDs(snapshot domain.Snapshot) []string {
	ids := make([]string, 0, len(snapshot.Drives))
	for _, d := range snapshot.Drives {
		ids = append(ids, displayID(d))
	}
	sort.Strings(ids)
	return ids
}

func uniqueDrive(matches []domain.Drive) (domain.Drive, bool) {
	if len(matches) == 1 {
		return matches[0], true
	}
	return domain.Drive{}, false
}
func sameBay(a, b domain.Bay) bool {
	enclosureA, enclosureB := a.Enclosure, b.Enclosure
	if a.EnclosureIdentity != "" && b.EnclosureIdentity != "" {
		enclosureA, enclosureB = a.EnclosureIdentity, b.EnclosureIdentity
	}
	if !strings.EqualFold(enclosureA, enclosureB) {
		return false
	}
	sameLabel := a.Label != "" && b.Label != "" && strings.EqualFold(a.Label, b.Label)
	sameSlot := a.SlotKnown && b.SlotKnown && a.Slot == b.Slot
	return sameLabel || sameSlot
}
func displayID(d domain.Drive) string {
	if d.ID != "" {
		return d.ID
	}
	if d.WWN != "" {
		return d.WWN
	}
	if d.Serial != "" {
		return d.Serial
	}
	return d.DevNode
}
