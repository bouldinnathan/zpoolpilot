// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package planner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

const tebibyte = uint64(1024 * 1024 * 1024 * 1024)

var fixedNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func testBuilder() *Builder {
	return &Builder{
		Now:   func() time.Time { return fixedNow },
		NewID: func() (string, error) { return "00112233445566778899aabbccddeeff", nil },
	}
}

func testDrive(letter string, index int) domain.Drive {
	wwn := "5000c5000000000" + letter
	return domain.Drive{
		ID: "wwn:" + wwn, WWN: wwn, Serial: "SERIAL-" + strings.ToUpper(letter),
		ByID: "/dev/disk/by-id/wwn-" + wwn, DevNode: "/dev/sd" + letter,
		SysfsPath:     "/sys/dev/block/8:" + string(rune('0'+index)),
		CapacityBytes: 10 * tebibyte, LogicalSectorBytes: 512, PhysicalSectorBytes: 4096,
	}
}

func idleTrim() domain.TrimStats {
	supported := true
	return domain.TrimStats{State: "untrimmed", Supported: &supported}
}

func createSnapshot() domain.Snapshot {
	a, b, c, d := testDrive("a", 1), testDrive("b", 2), testDrive("c", 3), testDrive("d", 4)
	return domain.Snapshot{
		APIVersion: domain.SnapshotAPIVersion, Generated: fixedNow,
		Host:   domain.Host{MachineID: "machine-1", Hostname: "zfs-host"},
		Drives: []domain.Drive{a, b, c, d},
	}
}

func mirrorSnapshot() domain.Snapshot {
	snapshot := createSnapshot()
	snapshot.Drives[0].InUse, snapshot.Drives[0].Use = true, "pool member tank"
	snapshot.Drives[1].InUse, snapshot.Drives[1].Use = true, "pool member tank"
	leaves := []domain.VDev{
		{Name: "wwn-a", GUID: "leaf-a", Type: "disk", StableDriveID: snapshot.Drives[0].ID, SizeBytes: 9 * tebibyte, State: "ONLINE"},
		{Name: "wwn-b", GUID: "leaf-b", Type: "disk", StableDriveID: snapshot.Drives[1].ID, SizeBytes: 9 * tebibyte, State: "ONLINE"},
	}
	snapshot.Pools = []domain.Pool{{
		Name: "tank", GUID: "pool-1", State: "ONLINE",
		Scan:  domain.Scan{State: "none"},
		Trim:  idleTrim(),
		VDevs: []domain.VDev{{Name: "mirror-0", GUID: "mirror-1", Type: "mirror", Class: "data", State: "ONLINE", Children: leaves}},
	}}
	return snapshot
}

func raidzSnapshot() domain.Snapshot {
	snapshot := createSnapshot()
	snapshot.Host.OpenZFSVersion = "2.3.4"
	children := make([]domain.VDev, 3)
	for index := range 3 {
		drive := snapshot.Drives[index]
		drive.InUse, drive.Use = true, "pool member rz"
		snapshot.Drives[index] = drive
		children[index] = domain.VDev{Name: drive.ID, GUID: "rz-leaf-" + drive.ID, Type: "disk", StableDriveID: drive.ID, SizeBytes: 9 * tebibyte, State: "ONLINE"}
	}
	snapshot.Pools = []domain.Pool{{
		Name: "rz", GUID: "pool-rz", State: "ONLINE",
		Scan:  domain.Scan{State: "none"},
		Trim:  idleTrim(),
		VDevs: []domain.VDev{{Name: "raidz1-0", GUID: "raidz-guid", Type: "raidz1", Class: "data", State: "ONLINE", Children: children}},
	}}
	return snapshot
}

func bootSnapshot() domain.Snapshot {
	snapshot := createSnapshot()
	smartPassed := true
	snapshot.Host.ProxmoxVersion = "pve-manager/9.0.3/abcdef"
	snapshot.Host.ProxmoxNode = "pve-node-1"
	snapshot.Host.ProxmoxBootMode = "grub"
	snapshot.Drives[0].InUse, snapshot.Drives[0].Use = true, "ZFS pool rpool"
	snapshot.Drives[0].Health = domain.DriveHealth{Severity: domain.SeverityOK, SMARTPassed: &smartPassed, FreshAt: fixedNow}
	snapshot.Drives[1].InUse, snapshot.Drives[1].Use = true, "ZFS pool rpool"
	snapshot.Pools = []domain.Pool{{
		Name: "rpool", GUID: "boot-pool-guid", State: "DEGRADED", BootCritical: true,
		Scan: domain.Scan{State: "none"},
		Trim: idleTrim(),
		VDevs: []domain.VDev{{
			Name: "mirror-0", GUID: "boot-mirror-guid", Type: "mirror", Class: "data", State: "DEGRADED",
			Children: []domain.VDev{
				{Name: "source-part3", GUID: "boot-source-vdev", Type: "disk", Path: snapshot.Drives[0].ByID + "-part3", StableDriveID: snapshot.Drives[0].ID, State: "ONLINE", SizeBytes: 9 * tebibyte},
				{Name: "failed-part3", GUID: "boot-old-vdev", Type: "disk", Path: snapshot.Drives[1].ByID + "-part3", StableDriveID: snapshot.Drives[1].ID, State: "FAULTED", SizeBytes: 9 * tebibyte},
			},
		}},
	}}
	return snapshot
}

func TestCompressionLevelValidation(t *testing.T) {
	tests := map[string]bool{
		"gzip-1": true, "gzip-9": true, "gzip-0": false, "gzip-10": false,
		"zstd-1": true, "zstd-19": true, "zstd-0": false, "zstd-20": false,
	}
	for value, want := range tests {
		if got := validCompression(value); got != want {
			t.Errorf("validCompression(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestCreatePoolPlanBuildsExactSafePreview(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, err := testBuilder().CreatePool(CreatePoolRequest{
		Snapshot: snapshot, PoolName: "tank", Topology: TopologyMirror,
		Drives: []domain.Drive{snapshot.Drives[0], snapshot.Drives[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", plan.Blockers)
	}
	want := domain.PlannedCommand{
		Program: "zpool", Risk: domain.RiskDestructive,
		Args:   []string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-O", "dedup=off", "tank", "mirror", "/dev/disk/by-id/wwn-5000c5000000000a", "/dev/disk/by-id/wwn-5000c5000000000b"},
		Reason: "create the planned pool using persistent drive paths",
	}
	if len(plan.Commands) != 1 || !reflect.DeepEqual(plan.Commands[0], want) {
		t.Fatalf("command = %#v, want %#v", plan.Commands, want)
	}
	if plan.Confirmation != "yes "+Fingerprint(plan)[:12] {
		t.Fatalf("confirmation = %q", plan.Confirmation)
	}
	if err := ValidatePlan(plan, fixedNow); err != nil {
		t.Fatalf("ValidatePlan() error = %v", err)
	}
}

func TestCreatePoolSafetyBlockers(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	inUse := snapshot.Drives[0]
	inUse.InUse, inUse.Use = true, "mounted at /data"
	snapshot.Drives[0] = inUse
	plan, err := testBuilder().CreatePool(CreatePoolRequest{
		Snapshot: snapshot, PoolName: "-bad", Topology: TopologySingle, Drives: []domain.Drive{inUse},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Blockers, "\n")
	for _, want := range []string{"pool name", "expert mode", "already in use"} {
		if !strings.Contains(joined, want) {
			t.Errorf("blockers %q do not contain %q", joined, want)
		}
	}
	if len(plan.Commands) != 0 {
		t.Fatalf("invalid pool name unexpectedly produced command: %#v", plan.Commands)
	}
}

func TestAllLifecycleConstructors(t *testing.T) {
	t.Parallel()
	t.Run("prepare", func(t *testing.T) {
		snapshot := createSnapshot()
		plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[2]})
		if err != nil || len(plan.Blockers) != 0 || len(plan.Commands) != 2 {
			t.Fatalf("PrepareDisk() = blockers %v, commands %v, err %v", plan.Blockers, plan.Commands, err)
		}
		if plan.Commands[0].Program != "sgdisk" || plan.Commands[1].Program != "wipefs" {
			t.Fatalf("unexpected prepare sequence: %#v", plan.Commands)
		}
	})
	t.Run("replace", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
		if err != nil || len(plan.Blockers) != 0 {
			t.Fatalf("ReplaceDisk() blockers %v, err %v", plan.Blockers, err)
		}
		want := []string{"replace", "tank", "leaf-a", "/dev/disk/by-id/wwn-5000c5000000000c"}
		if !reflect.DeepEqual(plan.Commands[0].Args, want) {
			t.Fatalf("replace args = %v, want %v", plan.Commands[0].Args, want)
		}
	})
	t.Run("attach mirror", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		plan, err := testBuilder().AttachMirror(AttachMirrorRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], ExistingVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2]})
		if err != nil || len(plan.Blockers) != 0 || plan.Commands[0].Args[0] != "attach" {
			t.Fatalf("AttachMirror() = %#v, %v", plan, err)
		}
	})
	t.Run("expand raidz", func(t *testing.T) {
		snapshot := raidzSnapshot()
		plan, err := testBuilder().ExpandRAIDZ(ExpandRAIDZRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], RAIDZVDev: snapshot.Pools[0].VDevs[0], NewDrive: snapshot.Drives[3]})
		if err != nil || len(plan.Blockers) != 0 || plan.Commands[0].Args[0] != "attach" {
			t.Fatalf("ExpandRAIDZ() blockers %v, commands %v, err %v", plan.Blockers, plan.Commands, err)
		}
	})
	t.Run("matching add vdev", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		plan, err := testBuilder().AddVDev(AddVDevRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], Topology: TopologyMirror, Drives: []domain.Drive{snapshot.Drives[2], snapshot.Drives[3]}})
		if err != nil || len(plan.Blockers) != 0 {
			t.Fatalf("AddVDev() blockers %v, err %v", plan.Blockers, err)
		}
		want := []string{"add", "tank", "mirror", "/dev/disk/by-id/wwn-5000c5000000000c", "/dev/disk/by-id/wwn-5000c5000000000d"}
		if !reflect.DeepEqual(plan.Commands[0].Args, want) {
			t.Fatalf("add args = %v, want %v", plan.Commands[0].Args, want)
		}
	})
	t.Run("add spare", func(t *testing.T) {
		snapshot := mirrorSnapshot()
		plan, err := testBuilder().AddSpare(AddSpareRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], Drives: []domain.Drive{snapshot.Drives[2]}})
		if err != nil || len(plan.Blockers) != 0 {
			t.Fatalf("AddSpare() blockers %v, err %v", plan.Blockers, err)
		}
		if got := plan.Commands[0].Args; !reflect.DeepEqual(got, []string{"add", "tank", "spare", "/dev/disk/by-id/wwn-5000c5000000000c"}) {
			t.Fatalf("spare args = %v", got)
		}
	})
}

func TestPrepareAllowsInactiveSignaturesButBlocksActiveUse(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	snapshot.Drives[0].InUse = true
	snapshot.Drives[0].Use = "/dev/sda contains ext4"
	inactive, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil || len(inactive.Blockers) != 0 {
		t.Fatalf("inactive signature plan blockers=%v err=%v", inactive.Blockers, err)
	}
	snapshot.Drives[0].Use = "/dev/sda1 mounted at /srv"
	active, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(active.Blockers, " "), "actively in use") {
		t.Fatalf("active use was not blocked: %v", active.Blockers)
	}
	snapshot.Drives[0].Use = "/dev/sda contains zfs_member"
	rawOwner, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rawOwner.Blockers, " "), "cannot be proven inactive") {
		t.Fatalf("raw ownership signature was not blocked: %v", rawOwner.Blockers)
	}
}

func TestEncryptedCreateUsesInteractiveHiddenExecution(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, err := testBuilder().CreatePool(CreatePoolRequest{
		Snapshot: snapshot, PoolName: "secure", Topology: TopologyMirror,
		Drives:            []domain.Drive{snapshot.Drives[0], snapshot.Drives[1]},
		DatasetProperties: map[string]string{"encryption": "on", "keyformat": "passphrase", "keylocation": "prompt"},
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("encrypted plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false, execx.FakeResponse{Stdout: []byte("must not capture")})
	options := applyOptions(snapshot, runner)
	result, err := Apply(context.Background(), plan, options)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 1 || !calls[0].Interactive {
		t.Fatalf("encrypted create was not interactive: %#v", calls)
	}
	if len(result.Commands) != 1 || !result.Commands[0].OutputHidden {
		t.Fatalf("interactive trace not hidden: %#v", result.Commands)
	}
}

func TestReplaceBootDiskBuildsFixedProxmoxSequence(t *testing.T) {
	t.Parallel()
	snapshot := bootSnapshot()
	old := snapshot.Pools[0].VDevs[0].Children[1]
	plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: old,
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("unexpected boot blockers: %v", plan.Blockers)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "no current recognized SMART health sample") {
		t.Fatalf("unknown replacement health was not warned: %v", plan.Warnings)
	}
	want := []domain.PlannedCommand{
		{Program: "sgdisk", Args: []string{"/dev/disk/by-id/wwn-5000c5000000000a", "-R", "/dev/disk/by-id/wwn-5000c5000000000c"}, Risk: domain.RiskDestructive, Reason: "clone the healthy Proxmox boot disk partition table onto the replacement whole disk"},
		{Program: "sgdisk", Args: []string{"-G", "/dev/disk/by-id/wwn-5000c5000000000c"}, Risk: domain.RiskDestructive, Reason: "randomize the replacement disk and partition GUIDs"},
		{Program: "partprobe", Args: []string{"/dev/disk/by-id/wwn-5000c5000000000c"}, Risk: domain.RiskDestructive, Reason: "ask the kernel to read the replacement partition table"},
		{Program: "proxmox-boot-tool", Args: []string{"format", "/dev/disk/by-id/wwn-5000c5000000000c-part2"}, Risk: domain.RiskDestructive, Reason: "format the fixed Proxmox installer ESP partition 2"},
		{Program: "proxmox-boot-tool", Args: []string{"init", "/dev/disk/by-id/wwn-5000c5000000000c-part2", "grub"}, Risk: domain.RiskDestructive, Reason: "initialize the replacement ESP in the explicitly selected boot mode"},
		{Program: "zpool", Args: []string{"replace", "rpool", "boot-old-vdev", "/dev/disk/by-id/wwn-5000c5000000000c-part3"}, Risk: domain.RiskDestructive, Reason: "replace the old boot-pool leaf using fixed Proxmox installer ZFS partition 3"},
	}
	if !reflect.DeepEqual(plan.Commands, want) {
		t.Fatalf("boot commands:\n got: %#v\nwant: %#v", plan.Commands, want)
	}
	for index, command := range plan.Commands {
		if strings.Contains(strings.Join(command.Args, " "), " -f") || strings.Contains(strings.Join(command.Args, " "), "/dev/sd") {
			t.Fatalf("unsafe command %d: %#v", index, command)
		}
	}
	if err := ValidatePlan(plan, fixedNow); err != nil {
		t.Fatalf("ValidatePlan() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "boot-plan.json")
	if err := SavePlanAt(path, plan, fixedNow); err != nil {
		t.Fatalf("SavePlanAt() error = %v", err)
	}
	loaded, err := LoadPlanAt(path, fixedNow.Add(time.Minute))
	if err != nil || !reflect.DeepEqual(loaded.Commands, plan.Commands) || !reflect.DeepEqual(loaded.SourceDrives, plan.SourceDrives) {
		t.Fatalf("boot plan persistence mismatch: loaded=%#v err=%v", loaded, err)
	}

	systemdSnapshot := snapshot
	systemdSnapshot.Host.ProxmoxBootMode = "systemd-boot"
	systemd, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: systemdSnapshot, Pool: systemdSnapshot.Pools[0], OldVDev: old,
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeSystemdBoot,
	})
	if err != nil || len(systemd.Blockers) != 0 {
		t.Fatalf("systemd boot plan blockers=%v err=%v", systemd.Blockers, err)
	}
	if got := systemd.Commands[4].Args; !reflect.DeepEqual(got, []string{"init", "/dev/disk/by-id/wwn-5000c5000000000c-part2"}) {
		t.Fatalf("systemd init args = %v", got)
	}
}

func TestReplaceBootDiskFailsClosedOnUnsupportedEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.Snapshot, *ReplaceBootDiskRequest)
		want   string
	}{
		{"plain grub", func(_ *domain.Snapshot, request *ReplaceBootDiskRequest) { request.BootMode = BootModePlainGRUB }, "plain GRUB"},
		{"missing Proxmox", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) { snapshot.Host.ProxmoxVersion = "" }, "Proxmox VE host evidence"},
		{"not boot critical", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) { snapshot.Pools[0].BootCritical = false }, "not marked boot-critical"},
		{"source not pool member", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[0].Use = "/dev/sda contains zfs_member"
		}, "not confirmed as an active member"},
		{"unhealthy source", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[0].Health.Severity = domain.SeverityCritical
		}, "healthy drive-health"},
		{"source has no overall SMART proof", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[0].Health.SMARTPassed = nil
		}, "healthy drive-health"},
		{"source not marked active", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[0].InUse = false
		}, "not marked in use"},
		{"target mounted", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[2].InUse = true
			snapshot.Drives[2].Use = "/dev/sdc1 mounted at /srv"
		}, "actively in use"},
		{"target has stale signature", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[2].InUse = true
			snapshot.Drives[2].Use = "/dev/sdc contains ext4"
		}, "run and verify prepare-disk first"},
		{"target too small", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[2].CapacityBytes = tebibyte
		}, "too small"},
		{"target hardware failure", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Drives[2].Health.Severity = domain.SeverityCritical
		}, "critical hardware health"},
		{"source no longer sibling", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Pools[0].VDevs[0].Children[0].StableDriveID = "wwn:other"
		}, "sibling candidates"},
		{"source volatile partition path", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Pools[0].VDevs[0].Children[0].Path = "/dev/sda3"
		}, "not pinned to persistent Proxmox ZFS partition 3"},
		{"old volatile partition path", func(snapshot *domain.Snapshot, _ *ReplaceBootDiskRequest) {
			snapshot.Pools[0].VDevs[0].Children[1].Path = "/dev/sdb3"
		}, "not pinned to persistent"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			snapshot := bootSnapshot()
			request := ReplaceBootDiskRequest{
				Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
				SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
			}
			test.mutate(&snapshot, &request)
			request.Snapshot = snapshot
			request.Pool = snapshot.Pools[0]
			request.OldVDev = snapshot.Pools[0].VDevs[0].Children[1]
			request.SourceDrive = snapshot.Drives[0]
			request.NewDrive = snapshot.Drives[2]
			plan, err := testBuilder().ReplaceBootDisk(request)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(plan.Blockers, " "), test.want) {
				t.Fatalf("blockers %v do not contain %q", plan.Blockers, test.want)
			}
		})
	}
}

func TestBootPlanRejectsModeAndCommandTampering(t *testing.T) {
	t.Parallel()
	snapshot := bootSnapshot()
	request := ReplaceBootDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
	}
	plan, _ := testBuilder().ReplaceBootDisk(request)
	plan.Desired["boot_mode"] = "grub; grub-install /dev/sda"
	plan.Confirmation = expectedRedConfirmation(plan)
	if _, err := RebuildCommands(plan); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("mode injection error = %v", err)
	}
	plan, _ = testBuilder().ReplaceBootDisk(request)
	plan.Commands[5].Args = append([]string{"replace", "-f"}, plan.Commands[5].Args[1:]...)
	if err := ValidatePlan(plan, fixedNow); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("command injection error = %v", err)
	}
}

func TestDedupRequiresExpertMode(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	request := CreatePoolRequest{
		Snapshot: snapshot, PoolName: "dedup", Topology: TopologyMirror,
		Drives:            []domain.Drive{snapshot.Drives[0], snapshot.Drives[1]},
		DatasetProperties: map[string]string{"dedup": "on"},
	}
	plan, err := testBuilder().CreatePool(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Blockers, " "), "dedup requires expert mode") {
		t.Fatalf("dedup did not block non-expert plan: %v", plan.Blockers)
	}
	request.Expert = true
	plan, err = testBuilder().CreatePool(request)
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("expert dedup blockers=%v err=%v", plan.Blockers, err)
	}
}

func TestCapacityTopologyAndBootBlockers(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	tooSmall := snapshot.Drives[2]
	tooSmall.CapacityBytes = tebibyte
	snapshot.Drives[2] = tooSmall
	replace, _ := testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: tooSmall})
	if !strings.Contains(strings.Join(replace.Blockers, " "), "too small") {
		t.Fatalf("missing capacity blocker: %v", replace.Blockers)
	}

	mismatch, _ := testBuilder().AddVDev(AddVDevRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], Topology: TopologyRAIDZ2, Drives: []domain.Drive{snapshot.Drives[2], snapshot.Drives[3]}})
	if !strings.Contains(strings.Join(mismatch.Blockers, " "), "does not match") {
		t.Fatalf("missing topology blocker: %v", mismatch.Blockers)
	}

	snapshot = mirrorSnapshot()
	snapshot.Pools[0].BootCritical = true
	boot, _ := testBuilder().ReplaceDisk(ReplaceDiskRequest{Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2], AllowBoot: true})
	joined := strings.Join(boot.Blockers, " ")
	if !strings.Contains(joined, "dedicated Proxmox boot-device plan") {
		t.Fatalf("boot pool was not explicitly blocked: %v", boot.Blockers)
	}

	oldZFS := raidzSnapshot()
	oldZFS.Host.OpenZFSVersion = "2.2.7"
	expand, _ := testBuilder().ExpandRAIDZ(ExpandRAIDZRequest{Snapshot: oldZFS, Pool: oldZFS.Pools[0], RAIDZVDev: oldZFS.Pools[0].VDevs[0], NewDrive: oldZFS.Drives[3]})
	if !strings.Contains(strings.Join(expand.Blockers, " "), "OpenZFS 2.3") {
		t.Fatalf("old OpenZFS expansion was not blocked: %v", expand.Blockers)
	}
}

func TestFingerprintCanonicalizationAndSafetySensitivity(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	reordered := snapshot
	reordered.Drives = append([]domain.Drive(nil), snapshot.Drives...)
	reordered.Pools = append([]domain.Pool(nil), snapshot.Pools...)
	for left, right := 0, len(reordered.Drives)-1; left < right; left, right = left+1, right-1 {
		reordered.Drives[left], reordered.Drives[right] = reordered.Drives[right], reordered.Drives[left]
	}
	reordered.Generated = fixedNow.Add(time.Hour)
	reordered.Commands = []domain.CommandTrace{{Program: "ignored"}}
	if SnapshotFingerprint(snapshot) != SnapshotFingerprint(reordered) || TopologyFingerprint(snapshot) != TopologyFingerprint(reordered) {
		t.Fatal("fingerprints changed for ordering or observational metadata")
	}
	reordered.Drives[0].InUse = !reordered.Drives[0].InUse
	if SnapshotFingerprint(snapshot) == SnapshotFingerprint(reordered) {
		t.Fatal("snapshot fingerprint ignored drive-use change")
	}
	if TopologyFingerprint(snapshot) != TopologyFingerprint(reordered) {
		t.Fatal("topology fingerprint changed for drive-use change")
	}
	stateChanged := snapshot
	stateChanged.Pools = append([]domain.Pool(nil), snapshot.Pools...)
	stateChanged.Pools[0].VDevs = append([]domain.VDev(nil), snapshot.Pools[0].VDevs...)
	stateChanged.Pools[0].VDevs[0].State = "DEGRADED"
	if SnapshotFingerprint(snapshot) == SnapshotFingerprint(stateChanged) {
		t.Fatal("snapshot fingerprint ignored vdev state change")
	}
	if TopologyFingerprint(snapshot) != TopologyFingerprint(stateChanged) {
		t.Fatal("topology fingerprint changed for vdev health state")
	}
}

func TestReplaceBlocksInadequateSurvivingRedundancy(t *testing.T) {
	t.Parallel()
	snapshot := mirrorSnapshot()
	snapshot.Pools[0].State = "DEGRADED"
	snapshot.Pools[0].VDevs[0].State = "DEGRADED"
	snapshot.Pools[0].VDevs[0].Children[0].State = "FAULTED"
	snapshot.Pools[0].VDevs[0].Children[1].State = "UNAVAIL"
	plan, err := testBuilder().ReplaceDisk(ReplaceDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[0], NewDrive: snapshot.Drives[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Blockers, " "), "no other ONLINE leaf") {
		t.Fatalf("missing redundancy blocker: %v", plan.Blockers)
	}
}

func TestPlanPersistenceStrictRoundTripTamperAndExpiry(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	directory := t.TempDir()
	path := filepath.Join(directory, "plan.json")
	if err := SavePlanAt(path, plan, fixedNow); err != nil {
		t.Fatalf("SavePlanAt() error = %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plan mode = %v, err %v", info.Mode().Perm(), err)
	}
	loaded, err := LoadPlanAt(path, fixedNow.Add(time.Minute))
	if err != nil || Fingerprint(loaded) != Fingerprint(plan) {
		t.Fatalf("LoadPlanAt() = fingerprint %s, err %v", Fingerprint(loaded), err)
	}
	if _, err := LoadPlanAt(path, plan.ExpiresAt); !errors.Is(err, ErrPlanExpired) {
		t.Fatalf("expired LoadPlanAt() error = %v", err)
	}

	encoded, _ := os.ReadFile(path)
	unknown := strings.Replace(string(encoded), "{\n", "{\n  \"unexpected\": true,\n", 1)
	if err := os.WriteFile(path, []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlanAt(path, fixedNow); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field LoadPlanAt() error = %v", err)
	}
}

func TestValidatePlanRejectsCommandAndDesiredTampering(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	plan.Commands[0].Program = "sh"
	if err := ValidatePlan(plan, fixedNow); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("command tamper error = %v", err)
	}
	plan, _ = testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	plan.Desired["argv"] = []string{"destroy", "tank"}
	plan.Confirmation = expectedRedConfirmation(plan)
	if _, err := RebuildCommands(plan); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("desired injection error = %v", err)
	}
}

type testUnlocker struct{ unlocked bool }

func (unlocker *testUnlocker) Unlock() error { unlocker.unlocked = true; return nil }

type testLocker struct {
	locked   int
	unlocker *testUnlocker
	err      error
}

func (locker *testLocker) Lock(context.Context) (Unlocker, error) {
	locker.locked++
	if locker.err != nil {
		return nil, locker.err
	}
	locker.unlocker = &testUnlocker{}
	return locker.unlocker, nil
}

func applyOptions(snapshot domain.Snapshot, runner execx.Runner) ApplyOptions {
	return ApplyOptions{
		Runner: runner, Now: func() time.Time { return fixedNow.Add(time.Minute) }, IsTTY: func() bool { return true },
		Locker: &testLocker{}, FreshSnapshot: func(context.Context) (domain.Snapshot, error) { return snapshot, nil },
		Confirm:     Confirmations{Red: func(_ context.Context, request ConfirmationRequest) (string, error) { return request.Expected, nil }},
		BeforeAudit: func(context.Context, AuditRecord) error { return nil },
		AfterAudit:  func(context.Context, AuditRecord) error { return nil },
		CommandGuard: func(context.Context, domain.ActionPlan, domain.PlannedCommand, int) error {
			return nil
		},
	}
}

func TestApplyPrepareRevalidatesEveryCommandAndAudits(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	runner := execx.NewFakeRunner(false, execx.FakeResponse{}, execx.FakeResponse{})
	options := applyOptions(snapshot, runner)
	var mu sync.Mutex
	snapshots, redPrompts, beforeAudits, afterAudits := 0, 0, 0, 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		snapshots++
		return snapshot, nil
	}
	options.Confirm.Red = func(_ context.Context, request ConfirmationRequest) (string, error) {
		redPrompts++
		if request.Expected != plan.Confirmation {
			t.Errorf("expected prompt %q, want %q", request.Expected, plan.Confirmation)
		}
		return request.Expected, nil
	}
	options.BeforeAudit = func(_ context.Context, record AuditRecord) error {
		beforeAudits++
		if record.Phase != "before" || record.Before == nil {
			t.Errorf("bad before audit: %#v", record)
		}
		return nil
	}
	options.AfterAudit = func(_ context.Context, record AuditRecord) error {
		afterAudits++
		if record.Phase != "after" || len(record.Commands) != 2 || record.After == nil {
			t.Errorf("bad after audit: %#v", record)
		}
		return nil
	}
	result, err := Apply(context.Background(), plan, options)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(result.Commands) != 2 || snapshots != 4 || redPrompts != 2 || beforeAudits != 1 || afterAudits != 1 {
		t.Fatalf("Apply counts: commands=%d snapshots=%d red=%d before=%d after=%d", len(result.Commands), snapshots, redPrompts, beforeAudits, afterAudits)
	}
	locker := options.Locker.(*testLocker)
	if locker.locked != 1 || !locker.unlocker.unlocked {
		t.Fatalf("lock lifecycle = %#v", locker)
	}
}

func TestApplyPreservesCallbackContextCauses(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("prepare plan blockers=%v err=%v", plan.Blockers, err)
	}
	tests := []struct {
		name    string
		cause   error
		wrapper error
		mutate  func(*ApplyOptions)
	}{
		{
			name: "fresh snapshot", cause: context.Canceled, wrapper: ErrRevalidation,
			mutate: func(options *ApplyOptions) {
				options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
					return domain.Snapshot{}, context.Canceled
				}
			},
		},
		{
			name: "confirmation", cause: context.DeadlineExceeded, wrapper: ErrConfirmation,
			mutate: func(options *ApplyOptions) {
				options.Confirm.Red = func(context.Context, ConfirmationRequest) (string, error) {
					return "", context.DeadlineExceeded
				}
			},
		},
		{
			name: "before audit", cause: context.Canceled, wrapper: ErrAudit,
			mutate: func(options *ApplyOptions) {
				options.BeforeAudit = func(context.Context, AuditRecord) error { return context.Canceled }
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			options := applyOptions(snapshot, execx.NewFakeRunner(false))
			test.mutate(&options)
			_, applyErr := Apply(context.Background(), plan, options)
			if !errors.Is(applyErr, test.wrapper) || !errors.Is(applyErr, test.cause) {
				t.Fatalf("Apply() error = %v, want wrapper %v and cause %v", applyErr, test.wrapper, test.cause)
			}
		})
	}
}

func TestApplyFailsClosedBeforeDispatch(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	base, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	tests := []struct {
		name    string
		mutate  func(*domain.ActionPlan, *ApplyOptions)
		wantErr error
	}{
		{"non tty", func(_ *domain.ActionPlan, options *ApplyOptions) { options.IsTTY = func() bool { return false } }, ErrTTYRequired},
		{"blocker", func(plan *domain.ActionPlan, _ *ApplyOptions) {
			plan.Blockers = []string{"unsafe"}
			plan.Confirmation = expectedRedConfirmation(*plan)
		}, ErrPlanBlocked},
		{"expired", func(plan *domain.ActionPlan, options *ApplyOptions) {
			options.Now = func() time.Time { return plan.ExpiresAt }
		}, ErrPlanExpired},
		{"missing audit", func(_ *domain.ActionPlan, options *ApplyOptions) { options.BeforeAudit = nil }, ErrAudit},
		{"bad confirmation", func(_ *domain.ActionPlan, options *ApplyOptions) {
			options.Confirm.Red = func(context.Context, ConfirmationRequest) (string, error) { return "yes", nil }
		}, ErrConfirmation},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			plan := base
			plan.Blockers = append([]string(nil), base.Blockers...)
			plan.Commands = append([]domain.PlannedCommand(nil), base.Commands...)
			runner := execx.NewFakeRunner(false)
			options := applyOptions(snapshot, runner)
			test.mutate(&plan, &options)
			_, err := Apply(context.Background(), plan, options)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Apply() error = %v, want %v", err, test.wantErr)
			}
			if len(runner.Calls()) != 0 {
				t.Fatalf("unsafe Apply dispatched calls: %#v", runner.Calls())
			}
		})
	}
}

func TestApplyCatchesDriveSwapImmediatelyBeforeCommand(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	calls := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		calls++
		if calls == 2 {
			changed := snapshot
			changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
			changed.Drives[0].Serial = "SWAPPED"
			return changed, nil
		}
		return snapshot, nil
	}
	_, err := Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) && !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("Apply() error = %v, want revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("drive swap dispatched calls: %#v", runner.Calls())
	}
}

func TestApplyRevalidatesBetweenPrepareCommands(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	runner := execx.NewFakeRunner(false, execx.FakeResponse{})
	options := applyOptions(snapshot, runner)
	snapshotCall := 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		snapshotCall++
		if snapshotCall == 3 {
			changed := snapshot
			changed.Drives = append([]domain.Drive(nil), snapshot.Drives...)
			changed.Drives[0].SysfsPath = "/sys/dev/block/8:99"
			return changed, nil
		}
		return snapshot, nil
	}
	result, err := Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error = %v, want ErrRevalidation", err)
	}
	if len(result.Commands) != 1 || len(runner.Calls()) != 1 {
		t.Fatalf("dispatched %d commands, result has %d", len(runner.Calls()), len(result.Commands))
	}
}

func TestApplyReadOnlyRunnerRejectsMutationAndWritesAfterAudit(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, _ := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	runner := execx.NewFakeRunner(true, execx.FakeResponse{})
	options := applyOptions(snapshot, runner)
	afterCalled := false
	options.AfterAudit = func(_ context.Context, record AuditRecord) error {
		afterCalled = true
		if !strings.Contains(record.Error, execx.ErrReadOnly.Error()) {
			t.Errorf("after audit error = %q", record.Error)
		}
		return nil
	}
	_, err := Apply(context.Background(), plan, options)
	if !errors.Is(err, execx.ErrReadOnly) {
		t.Fatalf("Apply() error = %v, want ErrReadOnly", err)
	}
	if !afterCalled || len(runner.Calls()) != 0 {
		t.Fatalf("afterCalled=%v dispatched=%d", afterCalled, len(runner.Calls()))
	}
}

func TestApplyBootReplacementRevalidatesEveryStepAndKeepsReplaceLast(t *testing.T) {
	t.Parallel()
	snapshot := bootSnapshot()
	plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
		Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
		SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeGRUB,
	})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("boot plan blockers=%v err=%v", plan.Blockers, err)
	}
	responses := make([]execx.FakeResponse, len(plan.Commands))
	runner := execx.NewFakeRunner(false, responses...)
	options := applyOptions(snapshot, runner)
	snapshotCalls, confirmations, guards := 0, 0, 0
	options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
		snapshotCalls++
		if snapshotCalls >= 3 {
			prepared := snapshot
			prepared.Drives = append([]domain.Drive(nil), snapshot.Drives...)
			prepared.Drives[2].InUse = true
			prepared.Drives[2].Use = "/dev/sdc2 contains vfat, /dev/sdc3 contains zfs_member"
			return prepared, nil
		}
		return snapshot, nil
	}
	options.Confirm.Red = func(_ context.Context, request ConfirmationRequest) (string, error) {
		confirmations++
		return request.Expected, nil
	}
	options.CommandGuard = func(_ context.Context, _ domain.ActionPlan, command domain.PlannedCommand, index int) error {
		guards++
		if !reflect.DeepEqual(command, plan.Commands[index-1]) {
			t.Errorf("guard command %d mismatch: %+v", index, command)
		}
		return nil
	}
	result, err := Apply(context.Background(), plan, options)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 6 || len(result.Commands) != 6 || snapshotCalls != 8 || confirmations != 6 || guards != 6 {
		t.Fatalf("calls=%d traces=%d snapshots=%d confirmations=%d guards=%d", len(calls), len(result.Commands), snapshotCalls, confirmations, guards)
	}
	for index, call := range calls {
		wantInteractive := index == 3 || index == 4
		if call.Interactive != wantInteractive {
			t.Errorf("call %d interactive=%v want %v", index, call.Interactive, wantInteractive)
		}
	}
	if calls[5].Program != "zpool" || calls[5].Args[0] != "replace" {
		t.Fatalf("topology-changing command is not last: %#v", calls)
	}
	if !result.Commands[3].OutputHidden || !result.Commands[4].OutputHidden {
		t.Fatalf("interactive Proxmox traces are not hidden: %#v", result.Commands)
	}
}

func TestApplyDestructiveOperationsRequireLiveCommandGuard(t *testing.T) {
	t.Parallel()
	snapshot := createSnapshot()
	plan, err := testBuilder().PrepareDisk(PrepareDiskRequest{Snapshot: snapshot, Drive: snapshot.Drives[0]})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("prepare plan blockers=%v err=%v", plan.Blockers, err)
	}
	runner := execx.NewFakeRunner(false)
	options := applyOptions(snapshot, runner)
	options.CommandGuard = nil
	_, err = Apply(context.Background(), plan, options)
	if !errors.Is(err, ErrRevalidation) {
		t.Fatalf("Apply() error=%v, want live guard revalidation failure", err)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("destructive commands dispatched without live guard: %+v", runner.Calls())
	}
}

func TestApplyBootReplacementStopsOnMidPlanChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		changeAt       int
		change         func(domain.Snapshot) domain.Snapshot
		wantDispatched int
	}{
		{
			name: "source device swap", changeAt: 5, wantDispatched: 3,
			change: func(snapshot domain.Snapshot) domain.Snapshot {
				snapshot.Drives = append([]domain.Drive(nil), snapshot.Drives...)
				snapshot.Drives[0].MajorMinor = "8:99"
				return snapshot
			},
		},
		{
			name: "source sibling leaves ONLINE state", changeAt: 5, wantDispatched: 3,
			change: func(snapshot domain.Snapshot) domain.Snapshot {
				snapshot.Pools = append([]domain.Pool(nil), snapshot.Pools...)
				snapshot.Pools[0].VDevs = append([]domain.VDev(nil), snapshot.Pools[0].VDevs...)
				snapshot.Pools[0].VDevs[0].Children = append([]domain.VDev(nil), snapshot.Pools[0].VDevs[0].Children...)
				snapshot.Pools[0].VDevs[0].Children[0].State = "DEGRADED"
				return snapshot
			},
		},
		{
			name: "target becomes mounted", changeAt: 3, wantDispatched: 1,
			change: func(snapshot domain.Snapshot) domain.Snapshot {
				snapshot.Drives = append([]domain.Drive(nil), snapshot.Drives...)
				snapshot.Drives[2].InUse = true
				snapshot.Drives[2].Use = "/dev/sdc2 mounted at /mnt/unexpected"
				return snapshot
			},
		},
		{
			name: "topology changes before final replace", changeAt: 7, wantDispatched: 5,
			change: func(snapshot domain.Snapshot) domain.Snapshot {
				snapshot.Pools = append([]domain.Pool(nil), snapshot.Pools...)
				snapshot.Pools[0].VDevs = append([]domain.VDev(nil), snapshot.Pools[0].VDevs...)
				snapshot.Pools[0].VDevs[0].GUID = "unexpected-topology"
				return snapshot
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			snapshot := bootSnapshot()
			snapshot.Host.ProxmoxBootMode = "systemd-boot"
			plan, err := testBuilder().ReplaceBootDisk(ReplaceBootDiskRequest{
				Snapshot: snapshot, Pool: snapshot.Pools[0], OldVDev: snapshot.Pools[0].VDevs[0].Children[1],
				SourceDrive: snapshot.Drives[0], NewDrive: snapshot.Drives[2], BootMode: BootModeSystemdBoot,
			})
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("boot plan blockers=%v err=%v", plan.Blockers, err)
			}
			responses := make([]execx.FakeResponse, len(plan.Commands))
			runner := execx.NewFakeRunner(false, responses...)
			options := applyOptions(snapshot, runner)
			freshCalls := 0
			options.FreshSnapshot = func(context.Context) (domain.Snapshot, error) {
				freshCalls++
				if freshCalls == test.changeAt {
					return test.change(snapshot), nil
				}
				return snapshot, nil
			}
			_, err = Apply(context.Background(), plan, options)
			if !errors.Is(err, ErrRevalidation) && !errors.Is(err, ErrPlanChanged) {
				t.Fatalf("Apply() error = %v, want revalidation failure", err)
			}
			if got := len(runner.Calls()); got != test.wantDispatched {
				t.Fatalf("dispatched %d commands, want %d", got, test.wantDispatched)
			}
		})
	}
}

func TestFileLockerExcludesConcurrentApply(t *testing.T) {
	t.Parallel()
	locker := FileLocker{Path: filepath.Join(t.TempDir(), "apply.lock")}
	first, err := locker.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := locker.Lock(ctx); !errors.Is(err, ErrLockHeld) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Lock() error = %v, want ErrLockHeld and DeadlineExceeded", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	third, err := locker.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock() after release error = %v", err)
	}
	_ = third.Unlock()
}
