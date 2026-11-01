// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
	"github.com/bouldinnathan/zpoolpilot/internal/inventory"
	"github.com/bouldinnathan/zpoolpilot/internal/monitor"
	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

func (a *app) runInventory(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 64, errors.New("inventory requires scan, list, mappings, map, verify, unmap, or locate")
	}
	switch args[0] {
	case "scan":
		fs := flag.NewFlagSet("inventory scan", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		jsonOut := fs.Bool("json", false, "emit SnapshotV1 JSON")
		wake := fs.Bool("wake", false, "allow wake-up for fresh SMART data")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 {
			return 64, errors.New("inventory scan takes no positional arguments")
		}
		if *wake {
			if a.readOnly {
				return 77, errors.New("--wake is unavailable in read-only mode")
			}
			ok, err := a.confirmYellowContext(ctx, "SMART queries may wake sleeping drives", []string{"smartctl", "-j", "-x", "DEVICE"})
			if err != nil || !ok {
				if err == nil {
					err = errors.New("scan declined")
				}
				return 1, err
			}
		}
		snapshot, collectErr, saveErr := a.collectAndSaveSnapshot(ctx, false, true, func(callCtx context.Context) (domain.Snapshot, error) {
			return a.collect(callCtx, "hardware", *wake, true)
		})
		contextErr := callerContextError(ctx, errors.Join(collectErr, saveErr))
		if *jsonOut {
			writeErr := writeJSON(a.out, snapshot)
			if saveErr != nil {
				return 74, errors.Join(fmt.Errorf("inventory cache update failed: %w", saveErr), writeErr, contextErr)
			}
			if contextErr != nil {
				return monitorCode(snapshot), errors.Join(contextErr, writeErr)
			}
			if collectErr != nil {
				return 69, errors.Join(collectErr, writeErr)
			}
			return monitorCode(snapshot), writeErr
		}
		printInventory(a, snapshot)
		if collectErr != nil {
			fmt.Fprintln(a.errOut, "zpoolpilot: partial collection:", terminalText(collectErr.Error()))
		}
		if saveErr != nil {
			return 74, errors.Join(fmt.Errorf("inventory cache update failed: %w", saveErr), contextErr)
		}
		if contextErr != nil {
			return monitorCode(snapshot), contextErr
		}
		if collectErr != nil {
			return 69, collectErr
		}
		return monitorCode(snapshot), nil
	case "list":
		fs := flag.NewFlagSet("inventory list", flag.ContinueOnError)
		fs.SetOutput(a.errOut)
		jsonOut := fs.Bool("json", false, "emit drive array JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return 64, err
		}
		if fs.NArg() != 0 {
			return 64, errors.New("inventory list takes no positional arguments")
		}
		snapshot, err := a.loadSnapshot(ctx)
		if err != nil {
			return 3, fmt.Errorf("no cached inventory; run inventory scan: %w", err)
		}
		if *jsonOut {
			return 0, writeJSON(a.out, snapshot.Drives)
		}
		printInventory(a, snapshot)
		return 0, nil
	case "mappings":
		return a.listBayMappings(ctx, args[1:])
	case "map":
		return a.mapBay(ctx, args[1:], false)
	case "verify":
		return a.mapBay(ctx, args[1:], true)
	case "unmap":
		return a.unmapBay(ctx, args[1:])
	case "locate":
		return a.locateBay(ctx, args[1:])
	default:
		return 64, fmt.Errorf("unknown inventory command %q", args[0])
	}
}

type authoritativeBayMapping struct {
	DriveID string     `json:"drive_id"`
	Bay     domain.Bay `json:"bay"`
}

func (a *app) listBayMappings(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("inventory mappings", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	jsonOut := fs.Bool("json", false, "emit authoritative mapping array JSON")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 {
		return 64, errors.New("inventory mappings takes no positional arguments")
	}
	mappings, err := inventory.OpenBayFile(a.cfg.BayMapPath).LoadBays(ctx)
	if errors.Is(err, inventory.ErrBayStoreMissing) {
		mappings = map[string]domain.Bay{}
	} else if err != nil {
		return 2, fmt.Errorf("load authoritative bay mappings: %w", err)
	}
	rows := sortedAuthoritativeBayMappings(mappings)
	if *jsonOut {
		return 0, writeJSON(a.out, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.out, "No authoritative manual bay mappings are stored.")
		return 0, nil
	}
	fmt.Fprintln(a.out, "AUTHORITATIVE DRIVE ID                         ENCLOSURE             BAY              SLOT      CONFIDENCE  VERIFIED")
	for _, row := range rows {
		verified := "-"
		if !row.Bay.VerifiedAt.IsZero() {
			verified = row.Bay.VerifiedAt.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(a.out, "%-46s %-21.21s %-16.16s %-9s %-11.11s %s\n",
			terminalText(row.DriveID), valueOrUnknown(row.Bay.Enclosure), terminalText(row.Bay.Label),
			baySlotText(row.Bay), terminalText(row.Bay.Confidence), verified)
	}
	return 0, nil
}

func sortedAuthoritativeBayMappings(mappings map[string]domain.Bay) []authoritativeBayMapping {
	ids := make([]string, 0, len(mappings))
	for driveID := range mappings {
		ids = append(ids, driveID)
	}
	sort.Strings(ids)
	rows := make([]authoritativeBayMapping, 0, len(ids))
	for _, driveID := range ids {
		rows = append(rows, authoritativeBayMapping{DriveID: driveID, Bay: mappings[driveID]})
	}
	return rows
}

func (a *app) unmapBay(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("inventory unmap", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	driveID := fs.String("drive", "", "exact stable drive ID shown by inventory mappings")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 || *driveID == "" {
		return 64, errors.New("usage: inventory unmap --drive EXACT_STORED_ID")
	}
	if a.readOnly {
		return 77, execx.ErrReadOnly
	}
	if err := requireBayMutationAuthority(os.Geteuid(), a.in != nil && isTTY(a.in)); err != nil {
		return 77, err
	}
	store := inventory.OpenBayFile(a.cfg.BayMapPath)
	mappings, err := store.LoadBays(ctx)
	if err != nil {
		return 2, fmt.Errorf("load authoritative bay mappings: %w", err)
	}
	expected, err := exactStoredBayMapping(mappings, *driveID)
	if err != nil {
		return 2, err
	}
	reason := fmt.Sprintf("retire exact stored drive ID %q from enclosure=%q bay=%q slot=%s", *driveID, expected.Enclosure, expected.Label, baySlotText(expected))
	ok, err := a.confirmYellowContext(ctx, reason, []string{"zpoolpilot", "inventory", "unmap", "--drive", *driveID})
	if err != nil {
		return 1, err
	}
	if !ok {
		return 1, errors.New("unmap declined")
	}
	currentMappings, err := store.LoadBays(ctx)
	if err != nil {
		return 2, fmt.Errorf("reload authoritative bay mappings after confirmation: %w", err)
	}
	current, err := exactStoredBayMapping(currentMappings, *driveID)
	if err != nil || !sameAuthoritativeBayMapping(current, expected) {
		return 2, errors.New("authoritative bay mapping changed after confirmation; unmap was refused")
	}
	if err := store.DeleteBay(ctx, *driveID, expected); err != nil {
		return 2, err
	}
	fmt.Fprintf(a.out, "unmapped exact stored drive ID %s from enclosure=%q bay=%q slot=%s\n",
		terminalText(*driveID), terminalText(expected.Enclosure), terminalText(expected.Label), baySlotText(expected))
	return 0, nil
}

func exactStoredBayMapping(mappings map[string]domain.Bay, driveID string) (domain.Bay, error) {
	if bay, present := mappings[driveID]; present {
		return bay, nil
	}
	return domain.Bay{}, fmt.Errorf("no mapping exists for exact stored drive ID %q; use `inventory mappings` and copy its wwn: or serial: key", driveID)
}

func sameAuthoritativeBayMapping(left, right domain.Bay) bool {
	return left.Enclosure == right.Enclosure && left.EnclosureIdentity == right.EnclosureIdentity &&
		left.Label == right.Label && left.Slot == right.Slot && left.SlotKnown == right.SlotKnown &&
		left.Source == right.Source && left.Confidence == right.Confidence && left.VerifiedAt.Equal(right.VerifiedAt)
}

func baySlotText(bay domain.Bay) string {
	if !bay.SlotKnown {
		return "unknown"
	}
	return strconv.Itoa(bay.Slot)
}

func (a *app) mapBay(ctx context.Context, args []string, verify bool) (int, error) {
	name := "inventory map"
	if verify {
		name = "inventory verify"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	driveID := fs.String("drive", "", "stable drive ID, WWN, serial, by-id, or current devnode")
	enclosure := fs.String("enclosure", "", "enclosure name or SES device path")
	bay := fs.String("bay", "", "human bay label")
	slot := fs.Int("slot", 0, "numeric enclosure slot when known")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 {
		return 64, errors.New(name + " takes flags only")
	}
	if *driveID == "" {
		return 64, errors.New("--drive is required")
	}
	if a.readOnly {
		return 77, execx.ErrReadOnly
	}
	if err := requireBayMutationAuthority(os.Geteuid(), a.in != nil && isTTY(a.in)); err != nil {
		return 77, err
	}
	snapshot, err := a.collect(ctx, "fast", false, false)
	if err != nil {
		return 3, fmt.Errorf("fresh inventory collection failed: %w", err)
	}
	if err := safetySnapshotError(snapshot); err != nil {
		return 3, err
	}
	drive, ok := inventory.FindDrive(snapshot, *driveID)
	if !ok {
		return 2, fmt.Errorf("drive %q did not resolve uniquely; present IDs: %s", *driveID, strings.Join(inventory.DriveIDs(snapshot), ", "))
	}
	if err := inventory.ValidateStableDriveIdentity(snapshot, drive); err != nil {
		return 2, fmt.Errorf("bay mapping requires stable hardware drive identity: %w", err)
	}
	bayProvided := flagWasProvided(fs, "bay")
	enclosureProvided := flagWasProvided(fs, "enclosure")
	slotKnown := flagWasProvided(fs, "slot")
	if verify {
		if !bayProvided {
			*bay = drive.Bay.Label
		}
		if !enclosureProvided {
			*enclosure = drive.Bay.Enclosure
		}
		if !slotKnown {
			*slot = drive.Bay.Slot
			slotKnown = drive.Bay.SlotKnown
		}
	}
	if *bay == "" {
		return 64, errors.New("--bay is required (or map the drive before verify)")
	}
	confidence := inventory.BayConfidenceInferred
	actionDescription := "write an inferred local bay mapping"
	resultVerb := "mapped"
	if verify {
		confidence = inventory.BayConfidenceVerified
		actionDescription = "write a verified local bay mapping"
		resultVerb = "verified"
	}
	var enclosureIdentity string
	if verify {
		if !slotKnown || *slot < 0 {
			return 64, errors.New("inventory verify requires an explicitly supplied or inherited nonnegative --slot")
		}
		enclosureIdentity, err = probeEnclosureIdentity(ctx, osEnclosureFileSystem{}, a.newRunner(true), *enclosure)
		if err != nil {
			return 2, fmt.Errorf("verify SES enclosure identity: %w", err)
		}
	}
	argv := []string{"inventory", name[strings.LastIndex(name, " ")+1:], "--drive", drive.ID, "--enclosure", *enclosure, "--bay", *bay}
	if slotKnown {
		argv = append(argv, "--slot", strconv.Itoa(*slot))
	}
	ok, err = a.confirmYellowContext(ctx, actionDescription, append([]string{"zpoolpilot"}, argv...))
	if err != nil {
		return 1, err
	}
	if !ok {
		return 1, errors.New("mapping declined")
	}
	currentSnapshot, collectErr := a.collect(ctx, "fast", false, false)
	if collectErr != nil {
		return 3, fmt.Errorf("immediate pre-mapping inventory collection failed: %w", collectErr)
	}
	if err := safetySnapshotError(currentSnapshot); err != nil {
		return 3, err
	}
	currentDrive, present := inventory.FindDrive(currentSnapshot, drive.ID)
	if !present {
		return 2, fmt.Errorf("drive %q is no longer present after confirmation; mapping was refused", drive.ID)
	}
	if !samePhysicalDriveIdentity(drive, currentDrive) {
		return 2, fmt.Errorf("drive %q identity changed after confirmation; mapping was refused", drive.ID)
	}
	if verify {
		currentIdentity, probeErr := probeEnclosureIdentity(ctx, osEnclosureFileSystem{}, a.newRunner(true), *enclosure)
		if probeErr != nil {
			return 2, fmt.Errorf("revalidate SES enclosure immediately before storing verification: %w", probeErr)
		}
		if currentIdentity != enclosureIdentity {
			return 2, fmt.Errorf("SES enclosure identity changed after confirmation from %q to %q; verification was refused", enclosureIdentity, currentIdentity)
		}
	}
	bayStore := inventory.OpenBayFile(a.cfg.BayMapPath)
	if err := inventory.MapBay(ctx, bayStore, currentSnapshot, currentDrive.ID, domain.Bay{
		Enclosure: *enclosure, EnclosureIdentity: enclosureIdentity, Label: *bay,
		Slot: *slot, SlotKnown: slotKnown, Confidence: confidence,
	}); err != nil {
		return 2, err
	}
	slotText := "unknown"
	if slotKnown {
		slotText = strconv.Itoa(*slot)
	}
	fmt.Fprintf(a.out, "%s %s (%s) as enclosure=%q bay=%q slot=%s confidence=%s", resultVerb, terminalText(drive.ID), terminalText(drive.Serial), terminalText(*enclosure), terminalText(*bay), slotText, confidence)
	if enclosureIdentity != "" {
		fmt.Fprintf(a.out, " enclosure-identity=%q", terminalText(enclosureIdentity))
	}
	fmt.Fprintln(a.out)
	return 0, nil
}

func (a *app) locateBay(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("inventory locate", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	driveID := fs.String("drive", "", "present-drive selector for on; exact stored drive ID for off")
	state := fs.String("state", "on", "on or off")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 || *driveID == "" {
		return 64, errors.New("usage: inventory locate --drive ID [--state on|off]")
	}
	if *state != "on" && *state != "off" {
		return 64, errors.New("--state must be on or off")
	}
	if a.readOnly {
		return 77, execx.ErrReadOnly
	}
	if err := requireBayMutationAuthority(os.Geteuid(), a.in != nil && isTTY(a.in)); err != nil {
		return 77, err
	}
	bayStore := inventory.OpenBayFile(a.cfg.BayMapPath)
	var drive domain.Drive
	mappingID := ""
	if *state == "on" {
		snapshot, err := a.collect(ctx, "fast", false, false)
		if err != nil {
			return 3, fmt.Errorf("fresh inventory collection failed: %w", err)
		}
		if err := safetySnapshotError(snapshot); err != nil {
			return 3, err
		}
		var ok bool
		drive, ok = inventory.FindDrive(snapshot, *driveID)
		if !ok {
			return 2, fmt.Errorf("drive %q did not resolve uniquely", *driveID)
		}
		if err := inventory.ValidateStableDriveIdentity(snapshot, drive); err != nil {
			return 2, fmt.Errorf("locate --state on requires stable hardware drive identity: %w", err)
		}
		mappingID = drive.ID
	} else {
		// Turning an LED off is a recovery operation: the disk may already be
		// gone. Use only the exact authenticated store key, never a devnode or
		// serial alias that would require guessing against current discovery.
		mappingID = *driveID
	}
	mappings, err := bayStore.LoadBays(ctx)
	if err != nil {
		return 2, fmt.Errorf("load authoritative bay mapping for %q: %w", mappingID, err)
	}
	bay, err := exactStoredBayMapping(mappings, mappingID)
	if err != nil {
		if *state == "off" {
			return 2, fmt.Errorf("locate --state off requires the exact stored drive ID: %w", err)
		}
		return 2, fmt.Errorf("drive %q has no authoritative bay mapping", mappingID)
	}
	if err := validateLocateBayForState(bay, *state); err != nil {
		return 2, err
	}
	identity, err := probeEnclosureIdentity(ctx, osEnclosureFileSystem{}, a.newRunner(true), bay.Enclosure)
	if err != nil {
		return 2, fmt.Errorf("revalidate SES enclosure identity: %w", err)
	}
	if identity != bay.EnclosureIdentity {
		return 2, fmt.Errorf("SES enclosure identity changed from %q to %q; physically re-verify the mapping", bay.EnclosureIdentity, identity)
	}
	action := "--set=ident"
	if *state == "off" {
		action = "--clear=ident"
	}
	argv := []string{"--dev-slot-num=" + strconv.Itoa(bay.Slot), action, bay.Enclosure}
	reason := fmt.Sprintf("turn identify LED %s for exact mapping %q at enclosure=%q bay=%q slot=%s", *state, mappingID, bay.Enclosure, bay.Label, baySlotText(bay))
	ok, err := a.confirmYellowContext(ctx, reason, append([]string{"sg_ses"}, argv...))
	if err != nil {
		return 1, err
	}
	if !ok {
		return 1, errors.New("locate declined")
	}
	if *state == "on" {
		currentSnapshot, collectErr := a.collect(ctx, "fast", false, false)
		if collectErr != nil {
			return 3, fmt.Errorf("immediate pre-locate inventory collection failed: %w", collectErr)
		}
		if err := safetySnapshotError(currentSnapshot); err != nil {
			return 3, err
		}
		currentDrive, present := inventory.FindDrive(currentSnapshot, drive.ID)
		if !present {
			return 2, fmt.Errorf("drive %q is no longer present after confirmation; locate was refused", drive.ID)
		}
		if err := inventory.ValidateStableDriveIdentity(currentSnapshot, currentDrive); err != nil {
			return 2, fmt.Errorf("revalidate stable drive identity immediately before locate: %w", err)
		}
		if !samePhysicalDriveIdentity(drive, currentDrive) {
			return 2, fmt.Errorf("drive %q identity changed after confirmation; locate was refused", drive.ID)
		}
	}
	identity, err = probeEnclosureIdentity(ctx, osEnclosureFileSystem{}, a.newRunner(true), bay.Enclosure)
	if err != nil {
		return 2, fmt.Errorf("revalidate SES enclosure immediately before locate: %w", err)
	}
	if identity != bay.EnclosureIdentity {
		return 2, fmt.Errorf("SES enclosure identity changed after confirmation from %q to %q; locate was refused", bay.EnclosureIdentity, identity)
	}
	// Reload after the potentially slow drive discovery and enclosure probe.
	// The command below is rebuilt only from this identical, freshly validated
	// record; a final quick probe then narrows the remaining device-node race.
	currentMappings, err := bayStore.LoadBays(ctx)
	if err != nil {
		return 2, fmt.Errorf("reload authoritative bay mapping immediately before locate: %w", err)
	}
	currentBay, present := currentMappings[mappingID]
	if !present || !sameAuthoritativeBayMapping(currentBay, bay) {
		return 2, errors.New("authoritative bay mapping changed after confirmation; locate was refused")
	}
	if err := validateLocateBayForState(currentBay, *state); err != nil {
		return 2, fmt.Errorf("revalidate bay mapping immediately before locate: %w", err)
	}
	identity, err = probeEnclosureIdentity(ctx, osEnclosureFileSystem{}, a.newRunner(true), currentBay.Enclosure)
	if err != nil {
		return 2, fmt.Errorf("final SES enclosure validation immediately before locate: %w", err)
	}
	if identity != currentBay.EnclosureIdentity {
		return 2, fmt.Errorf("SES enclosure identity changed after final mapping reload from %q to %q; locate was refused", currentBay.EnclosureIdentity, identity)
	}
	finalArgv := []string{"--dev-slot-num=" + strconv.Itoa(currentBay.Slot), action, currentBay.Enclosure}
	runner := a.newRunner(false)
	result, err := runner.Run(ctx, execx.Command{Program: "sg_ses", Args: finalArgv, Capability: execx.CapabilityMutate, Risk: domain.RiskChange, Timeout: a.cfg.CommandTimeout})
	if result.Trace.Program != "" {
		fmt.Fprintf(a.out, "%s %s exit=%d\n", riskLabel(a.color, domain.RiskChange), displayCommand(result.Trace.Program, result.Trace.Args), result.Trace.ExitCode)
	}
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(a.out, "identify LED %s for %s in %s/%s\n", terminalText(*state), terminalText(mappingID), terminalText(currentBay.Enclosure), terminalText(currentBay.Label))
	return 0, nil
}

func samePhysicalDriveIdentity(before, after domain.Drive) bool {
	return before.ID != "" && before.MajorMinor != "" &&
		planner.DriveIdentityFingerprint(before) == planner.DriveIdentityFingerprint(after) &&
		before.MajorMinor == after.MajorMinor
}

func flagWasProvided(flags *flag.FlagSet, name string) bool {
	provided := false
	flags.Visit(func(item *flag.Flag) {
		if item.Name == name {
			provided = true
		}
	})
	return provided
}

func validateLocateBay(bay domain.Bay) error {
	return validateLocateBayAt(bay, time.Now().UTC())
}

func validateLocateBayAt(bay domain.Bay, now time.Time) error {
	return validateLocateBayForStateAt(bay, "on", now)
}

func validateLocateBayForState(bay domain.Bay, state string) error {
	return validateLocateBayForStateAt(bay, state, time.Now().UTC())
}

func validateLocateBayForStateAt(bay domain.Bay, state string, now time.Time) error {
	if state != "on" && state != "off" {
		return errors.New("locate state must be on or off")
	}
	if bay.Source != "manual" || bay.Confidence != inventory.BayConfidenceVerified || bay.VerifiedAt.IsZero() {
		return errors.New("locate requires a manually verified bay mapping")
	}
	if !inventory.ValidSESEnclosureIdentity(bay.EnclosureIdentity) {
		return errors.New("locate requires a canonical non-placeholder SES enclosure identity")
	}
	if bay.VerifiedAt.After(now.Add(time.Minute)) {
		return errors.New("bay verification timestamp is implausibly far in the future")
	}
	if state == "on" && now.Sub(bay.VerifiedAt) > 15*time.Minute {
		return errors.New("locate --state on requires a physical bay verification from the last 15 minutes")
	}
	if bay.Label == "" || !bay.SlotKnown || bay.Slot < 0 {
		return errors.New("locate requires a verified bay label and explicitly known numeric slot")
	}
	if !inventory.ValidSESDevicePath(bay.Enclosure) {
		return errors.New("locate requires a direct /dev/sgN or /dev/bsg/H:C:T:L enclosure device path")
	}
	return nil
}

func requireBayMutationAuthority(euid int, tty bool) error {
	if euid != 0 {
		return errors.New("inventory map, verify, unmap, and locate must run as root")
	}
	if !tty {
		return errors.New("inventory map, verify, unmap, and locate require a real TTY")
	}
	return nil
}

func printInventory(a *app, snapshot domain.Snapshot) {
	fmt.Fprintln(a.out, "RISK                 BAY          DEVICE       TYPE       MAKE / MODEL                    SERIAL                 CAPACITY       USE")
	for _, d := range snapshot.Drives {
		bay := d.Bay.Label
		if bay == "" {
			bay = "UNKNOWN"
		}
		kind := d.MediaType
		if kind == "" {
			kind = "unknown"
		}
		if d.Transport != "" {
			kind += "/" + d.Transport
		}
		makeModel := strings.TrimSpace(strings.TrimSpace(d.Vendor) + " " + strings.TrimSpace(d.Model))
		use := "available"
		if d.InUse {
			use = d.Use
		}
		fmt.Fprintf(a.out, "%-20s %-12.12s %-12.12s %-10.10s %-31.31s %-22.22s %-14s %s\n", riskLabel(a.color, domain.RiskRead), terminalText(bay), terminalText(d.DevNode), terminalText(kind), terminalText(makeModel), valueOrUnknown(d.Serial), humanBytes(d.CapacityBytes), terminalText(use))
		fmt.Fprintf(a.out, "                     id=%s\n                     by-id=%s  device=%s\n", valueOrUnknown(d.ID), valueOrUnknown(d.ByID), valueOrUnknown(d.MajorMinor))
	}
}

func monitorCode(snapshot domain.Snapshot) int {
	return monitor.ExitCode(snapshot)
}
