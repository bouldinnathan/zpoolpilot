// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

type fakeRunner struct {
	mu            sync.Mutex
	responses     map[string]Result
	calls         []Command
	defaultResult Result
}

func (runner *fakeRunner) Run(_ context.Context, command Command) Result {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, command)
	if result, ok := runner.responses[commandKey(command.Program, command.Args...)]; ok {
		return result
	}
	if runner.defaultResult.Err != nil || runner.defaultResult.ExitCode != 0 || len(runner.defaultResult.Stdout) > 0 {
		return runner.defaultResult
	}
	return Result{ExitCode: -1, Err: exec.ErrNotFound}
}

func commandKey(program string, args ...string) string {
	return program + "\x00" + strings.Join(args, "\x00")
}

type fakeFileSystem struct {
	files       map[string][]byte
	directories map[string][]fs.DirEntry
	symlinks    map[string]string
}

func (filesystem fakeFileSystem) ReadFile(name string) ([]byte, error) {
	if value, ok := filesystem.files[name]; ok {
		return append([]byte(nil), value...), nil
	}
	return nil, fs.ErrNotExist
}

func (filesystem fakeFileSystem) ReadDir(name string) ([]fs.DirEntry, error) {
	if value, ok := filesystem.directories[name]; ok {
		return value, nil
	}
	return nil, fs.ErrNotExist
}

func (filesystem fakeFileSystem) EvalSymlinks(name string) (string, error) {
	if value, ok := filesystem.symlinks[name]; ok {
		return value, nil
	}
	return "", fs.ErrNotExist
}

type fakeDirEntry string

func (entry fakeDirEntry) Name() string               { return string(entry) }
func (entry fakeDirEntry) IsDir() bool                { return false }
func (entry fakeDirEntry) Type() fs.FileMode          { return fs.ModeSymlink }
func (entry fakeDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("not implemented") }

func TestCollectorBuildsCompleteSnapshot(t *testing.T) {
	fixedTime := time.Date(2026, 9, 3, 12, 34, 56, 0, time.UTC)
	runner := &fakeRunner{responses: map[string]Result{
		commandKey("zpool", "version", "-j"):                                                                                 {Stdout: []byte(`{"output_version":{"command":"zpool version","vers_major":0,"vers_minor":1},"zfs_version":{"userland":"zfs-2.3.4","kernel":"zfs-kmod-2.3.4"}}`)},
		commandKey("zpool", "status", "-j", "--json-int", "-P", "-t"):                                                        {Stdout: fixture(t, "zpool_status.json")},
		commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"): {Stdout: fixture(t, "zpool_list.json")},
		commandKey("zpool", "get", "-j", "--json-int", "all"):                                                                {Stdout: fixture(t, "zpool_get.json")},
		commandKey("zpool", "iostat", "-H", "-p"):                                                                            {Stdout: []byte("tank\t1820\t180\t10\t20\t30\t40\n")},
		commandKey("pveversion"):                                                                                             {Stdout: []byte("pve-manager/9.0.3/abcdef\n")},
		commandKey("proxmox-boot-tool", "status"):                                                                            {Stdout: []byte("ABCD-1234 is configured with: uefi (versions: 6.17)\n")},
		commandKey("zfs", "get", "-j", "--json-int", "all"):                                                                  {Stdout: fixture(t, "zfs_get.json")},
		commandKey("lsblk", "--json", "--bytes", "--paths", "--output", "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,VENDOR,REV,ROTA,TRAN,LOG-SEC,PHY-SEC,MOUNTPOINTS,FSTYPE,PKNAME"): {Stdout: fixture(t, "lsblk.json")},
		commandKey("udevadm", "info", "--query=property", "--name", "/dev/sda"):     {Stdout: []byte("ID_WWN=0x5000c5000000aaaa\nID_SERIAL_SHORT=SERIAL-A\nID_MODEL=ST1000\nID_BUS=ata\nDEVPATH=/devices/pci/sda\n")},
		commandKey("udevadm", "info", "--query=property", "--name", "/dev/sdb"):     {Stdout: []byte("ID_WWN=0x5000c5000000bbbb\nID_SERIAL_SHORT=SERIAL-B\nID_MODEL=ST1000\nID_BUS=scsi\nDEVPATH=/devices/pci/sdb\n")},
		commandKey("udevadm", "info", "--query=property", "--name", "/dev/nvme0n1"): {Stdout: []byte("ID_WWN=eui.0011223344556677\nID_SERIAL_SHORT=NVME-1\nID_MODEL=FAST\\x20NVME\nID_BUS=nvme\nDEVPATH=/devices/pci/nvme0/nvme0n1\n")},
		commandKey("smartctl", "--json=c", "--all", "/dev/sda"):                     {Stdout: fixture(t, "smart_ata.json")},
		commandKey("smartctl", "--json=c", "--all", "/dev/sdb"):                     {Stdout: fixture(t, "smart_sas.json"), ExitCode: 8},
		commandKey("smartctl", "--json=c", "--all", "/dev/nvme0n1"):                 {Stdout: fixture(t, "smart_nvme.json")},
	}}
	filesystem := fakeFileSystem{
		files: map[string][]byte{
			"/etc/machine-id":              []byte("machine-123\n"),
			"/etc/os-release":              []byte("NAME=Debian\nPRETTY_NAME=\"Proxmox VE 9.0\"\n"),
			"/proc/sys/kernel/osrelease":   []byte("6.14.0-zfs\n"),
			"/proc/self/mountinfo":         []byte("42 35 8:2 / / rw,relatime - ext4 /dev/sda2 rw\n"),
			"/proc/spl/kstat/zfs/arcstats": fixture(t, "arcstats.txt"),
		},
		directories: map[string][]fs.DirEntry{
			"/dev/disk/by-id": {
				fakeDirEntry("ata-ST1000_SERIAL-A"), fakeDirEntry("wwn-5000c5000000aaaa"),
				fakeDirEntry("wwn-5000c5000000aaaa-part1"), fakeDirEntry("wwn-5000c5000000bbbb"),
				fakeDirEntry("nvme-eui.0011223344556677"),
			},
			"/dev/disk/by-path": {
				fakeDirEntry("pci-0000:01:00.0-sas-phy2-lun-0"),
				fakeDirEntry("pci-0000:01:00.0-sas-phy2-lun-0-part1"),
			},
			"/sys/class/block":                 {fakeDirEntry("sda"), fakeDirEntry("sda1"), fakeDirEntry("sdb"), fakeDirEntry("sdb1"), fakeDirEntry("nvme0n1")},
			"/sys/class/block/sda/holders":     {},
			"/sys/class/block/sda1/holders":    {},
			"/sys/class/block/sdb/holders":     {},
			"/sys/class/block/sdb1/holders":    {},
			"/sys/class/block/nvme0n1/holders": {},
		},
		symlinks: map[string]string{
			"/dev/disk/by-id/ata-ST1000_SERIAL-A":               "/dev/sda",
			"/dev/disk/by-id/wwn-5000c5000000aaaa":              "/dev/sda",
			"/dev/disk/by-id/wwn-5000c5000000bbbb":              "/dev/sdb",
			"/dev/disk/by-id/nvme-eui.0011223344556677":         "/dev/nvme0n1",
			"/dev/disk/by-path/pci-0000:01:00.0-sas-phy2-lun-0": "/dev/sdb",
			"/sys/class/block/sda":                              "/sys/devices/pci/sda",
			"/sys/class/block/sdb":                              "/sys/devices/pci/sdb",
			"/sys/class/block/nvme0n1":                          "/sys/devices/pci/nvme0/nvme0n1",
		},
	}
	collector := New(runner, WithFileSystem(filesystem), WithClock(func() time.Time { return fixedTime }), WithHostname(func() (string, error) { return "pve-node-1", nil }))
	snapshot, err := collector.Collect(context.Background(), Options{IncludeCommandTrace: true, WakeSleepingDisks: true})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Partial || len(snapshot.Errors) != 0 {
		t.Fatalf("expected complete snapshot, partial=%v errors=%+v", snapshot.Partial, snapshot.Errors)
	}
	if snapshot.APIVersion == "" || snapshot.Generated != fixedTime || snapshot.Host.Hostname != "pve-node-1" || snapshot.Host.MachineID != "machine-123" || snapshot.Host.OpenZFSVersion != "2.3.4" || snapshot.Host.ProxmoxVersion != "pve-manager/9.0.3/abcdef" || snapshot.Host.ProxmoxNode != "pve-node-1" || snapshot.Host.ProxmoxBootMode != "systemd-boot" {
		t.Fatalf("unexpected snapshot metadata: %+v", snapshot)
	}
	if len(snapshot.Pools) != 1 || !snapshot.Pools[0].BootCritical || len(snapshot.Datasets) != 2 || len(snapshot.Drives) != 3 {
		t.Fatalf("missing collected records: pools=%+v datasets=%+v drives=%+v", snapshot.Pools, snapshot.Datasets, snapshot.Drives)
	}
	if got := snapshot.Pools[0].IO; got.ReadOperationsPerSecond != 10 || got.WriteOperationsPerSecond != 20 || got.ReadBytesPerSecond != 30 || got.WriteBytesPerSecond != 40 {
		t.Fatalf("iostat rates were not retained: %#v", got)
	}
	if got := snapshot.Pools[0].Trim; got.State != "mixed" || got.BytesDone != 750 || got.Errors != 2 {
		t.Fatalf("TRIM state from zpool status -t was not retained: %#v", got)
	}
	if snapshot.ARC.HitRatio != 0.8 {
		t.Errorf("ARC hit ratio = %v, want 0.8", snapshot.ARC.HitRatio)
	}
	byID := make(map[string]int)
	for index := range snapshot.Drives {
		byID[snapshot.Drives[index].ID] = index
	}
	sda, ok := byID["wwn:5000c5000000aaaa"]
	if !ok || snapshot.Drives[sda].ByID != "/dev/disk/by-id/wwn-5000c5000000aaaa" || pointerValue(snapshot.Drives[sda].Health.Pending) != 1 {
		t.Fatalf("SATA identity/health not retained: %+v", snapshot.Drives)
	}
	sdb, ok := byID["wwn:5000c5000000bbbb"]
	if !ok || snapshot.Drives[sdb].Bay.Label != "pci-0000:01:00.0-sas-phy2-lun-0" || snapshot.Drives[sdb].Bay.Confidence != "inferred" {
		t.Fatalf("by-path bay inference not retained: %+v", snapshot.Drives)
	}
	nvme, ok := byID["wwn:eui.0011223344556677"]
	if !ok || snapshot.Drives[nvme].Health.PercentageUsed == nil || *snapshot.Drives[nvme].Health.PercentageUsed != 92 {
		t.Fatalf("NVMe identity/health not retained: %+v", snapshot.Drives)
	}
	if !allLeafVDevsHaveIDs(snapshot.Pools[0].VDevs) {
		t.Fatalf("vdevs were not correlated to stable drive IDs: %+v", snapshot.Pools[0].VDevs)
	}
	if len(snapshot.Commands) != 15 {
		t.Fatalf("got %d command traces, want 15", len(snapshot.Commands))
	}
	for _, call := range runner.calls {
		if call.Capability != CapabilityObserve || call.Timeout <= 0 {
			t.Errorf("unsafe command metadata: %+v", call)
		}
		switch filepath.Base(call.Program) {
		case "zpool", "zfs", "lsblk", "udevadm", "smartctl", "pveversion", "proxmox-boot-tool":
		default:
			t.Errorf("unexpected program %q", call.Program)
		}
	}
}

func allLeafVDevsHaveIDs(vdevs []domain.VDev) bool {
	for _, vdev := range vdevs {
		if len(vdev.Children) > 0 {
			if !allLeafVDevsHaveIDs(vdev.Children) {
				return false
			}
			continue
		}
		if vdev.StableDriveID == "" {
			return false
		}
	}
	return true
}

func TestCollectorFallsBackWhenStatusJSONOmitsSafetyIdentity(t *testing.T) {
	runner := &fakeRunner{responses: map[string]Result{
		commandKey("zpool", "status", "-j", "--json-int", "-P", "-t"): {Stdout: fixture(t, "zpool_status_missing_leaf_path.json")},
		commandKey("zpool", "status", "-P", "-p", "-t"): {Stdout: []byte(`
  pool: tank
 state: ONLINE
  scan: none requested
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  /dev/sda  ONLINE       0     0     0

errors: No known data errors
`)},
		commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"): {Stdout: fixture(t, "zpool_list.json")},
		commandKey("zpool", "get", "-j", "--json-int", "all"):                                                                {Stdout: fixture(t, "zpool_get.json")},
		commandKey("zpool", "iostat", "-H", "-p"):                                                                            {Stdout: []byte("tank\t1820\t180\t10\t20\t30\t40\n")},
		commandKey("zfs", "get", "-j", "--json-int", "all"):                                                                  {Stdout: fixture(t, "zfs_get.json")},
	}}
	snapshot := domain.Snapshot{Errors: []domain.SourceError{}, Findings: []domain.Finding{}}
	state := collectionState{
		collector: New(runner),
		options:   (Options{}).withDefaults(),
		snapshot:  &snapshot,
	}
	state.collectZFS(context.Background())
	state.finish()

	if !snapshot.Partial {
		t.Fatal("fallback from incomplete status JSON must mark the snapshot partial")
	}
	foundParseError := false
	for _, sourceErr := range snapshot.Errors {
		if sourceErr.Source == "zpool.status.json" && sourceErr.Kind == "parse" && sourceErr.Partial {
			foundParseError = true
		}
	}
	if !foundParseError {
		t.Fatalf("missing status JSON fallback error: %+v", snapshot.Errors)
	}
	if len(snapshot.Pools) != 1 || len(snapshot.Pools[0].VDevs) != 1 || snapshot.Pools[0].VDevs[0].Path != "/dev/sda" {
		t.Fatalf("text fallback topology was not retained: %+v", snapshot.Pools)
	}
	fallbackKey := commandKey("zpool", "status", "-P", "-p", "-t")
	fallbackCalled := false
	for _, call := range runner.calls {
		if commandKey(call.Program, call.Args...) == fallbackKey {
			fallbackCalled = true
		}
	}
	if !fallbackCalled {
		t.Fatal("text zpool status fallback was not called")
	}
}

func TestPoolIdentityCoverageIsBidirectionalAndGUIDExact(t *testing.T) {
	expected := map[string]*domain.Pool{"tank": {Name: "tank", GUID: "111"}}
	tests := []struct {
		name     string
		observed map[string]string
	}{
		{"missing", map[string]string{}},
		{"extra", map[string]string{"tank": "111", "other": "222"}},
		{"GUID mismatch", map[string]string{"tank": "999"}},
		{"missing GUID", map[string]string{"tank": ""}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := poolIdentityCoverageError(expected, test.observed, "test source"); err == nil {
				t.Fatalf("identity coverage accepted %s observation: %#v", test.name, test.observed)
			}
		})
	}
	if err := poolIdentityCoverageError(expected, map[string]string{"tank": "111"}, "test source"); err != nil {
		t.Fatalf("matching identities rejected: %v", err)
	}
}

func TestIncompletePropertyJSONCannotHideBootPool(t *testing.T) {
	poolGetFallback := strings.Join([]string{
		"tank\tguid\t123456789\t-",
		"tank\tsize\t2000\t-",
		"tank\tallocated\t1820\t-",
		"tank\tfree\t180\t-",
		"tank\tfragmentation\t72\t-",
		"tank\tcapacity\t91\t-",
		"tank\thealth\tDEGRADED\t-",
		"tank\tbootfs\ttank/ROOT/default\tlocal",
	}, "\n") + "\n"
	zfsGetFallback := strings.Join([]string{
		"tank\ttype\tfilesystem\t-",
		"tank\tguid\t987654320\t-",
		"tank\tmountpoint\t/tank\tdefault",
		"tank/ROOT/default\ttype\tfilesystem\t-",
		"tank/ROOT/default\tguid\t987654321\t-",
		"tank/ROOT/default\tmountpoint\t/\tlocal",
	}, "\n") + "\n"

	tests := []struct {
		name            string
		poolGetJSON     []byte
		poolGetText     string
		zfsGetJSON      []byte
		zfsGetText      string
		wantErrorSource string
	}{
		{
			name:            "empty zpool get cannot erase bootfs",
			poolGetJSON:     []byte(`{"pools":{}}`),
			poolGetText:     poolGetFallback,
			zfsGetJSON:      fixture(t, "zfs_get.json"),
			wantErrorSource: "zpool.get.json",
		},
		{
			name:            "missing bootfs property cannot erase boot role",
			poolGetJSON:     []byte(`{"pools":{"tank":{"name":"tank","guid":123456789,"properties":{"guid":{"value":123456789},"size":{"value":2000},"allocated":{"value":1820},"free":{"value":180},"fragmentation":{"value":72},"capacity":{"value":91},"health":{"value":"DEGRADED"}}}}}`),
			poolGetText:     poolGetFallback,
			zfsGetJSON:      fixture(t, "zfs_get.json"),
			wantErrorSource: "zpool.get.json",
		},
		{
			name:            "same-name different-GUID zpool get is rejected",
			poolGetJSON:     []byte(strings.ReplaceAll(string(fixture(t, "zpool_get.json")), "123456789", "999999999")),
			poolGetText:     poolGetFallback,
			zfsGetJSON:      fixture(t, "zfs_get.json"),
			wantErrorSource: "zpool.get.json",
		},
		{
			name:            "empty zfs get cannot erase root mountpoint",
			poolGetJSON:     []byte(strings.Replace(string(fixture(t, "zpool_get.json")), "tank/ROOT/default", "none", 1)),
			zfsGetJSON:      []byte(`{"datasets":{}}`),
			zfsGetText:      zfsGetFallback,
			wantErrorSource: "zfs.get.json",
		},
		{
			name:        "missing mountpoint property cannot erase root mount",
			poolGetJSON: []byte(strings.Replace(string(fixture(t, "zpool_get.json")), "tank/ROOT/default", "none", 1)),
			zfsGetJSON: []byte(strings.Replace(
				string(fixture(t, "zfs_get.json")),
				`"mountpoint":{"value":"/","source":{"type":"LOCAL","data":"-"}},`,
				"",
				1,
			)),
			zfsGetText:      zfsGetFallback,
			wantErrorSource: "zfs.get.json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]Result{
				commandKey("zpool", "status", "-j", "--json-int", "-P", "-t"):                                                        {Stdout: fixture(t, "zpool_status.json")},
				commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"): {Stdout: fixture(t, "zpool_list.json")},
				commandKey("zpool", "get", "-j", "--json-int", "all"):                                                                {Stdout: test.poolGetJSON},
				commandKey("zpool", "get", "-H", "-p", "-o", "name,property,value,source", "all"):                                    {Stdout: []byte(test.poolGetText)},
				commandKey("zpool", "iostat", "-H", "-p"):                                                                            {Stdout: []byte("tank\t1820\t180\t10\t20\t30\t40\n")},
				commandKey("zfs", "get", "-j", "--json-int", "all"):                                                                  {Stdout: test.zfsGetJSON},
				commandKey("zfs", "get", "-H", "-p", "-o", "name,property,value,source", "all"):                                      {Stdout: []byte(test.zfsGetText)},
			}}
			snapshot := domain.Snapshot{Errors: []domain.SourceError{}, Findings: []domain.Finding{}, Datasets: []domain.Dataset{}}
			state := collectionState{collector: New(runner), options: (Options{}).withDefaults(), snapshot: &snapshot}
			state.collectZFS(context.Background())
			markBootPoolsFromDatasets(snapshot.Pools, snapshot.Datasets)
			state.finish()

			if len(snapshot.Pools) != 1 || !snapshot.Pools[0].BootCritical {
				t.Fatalf("incomplete JSON hid boot-critical pool: %+v; errors=%+v", snapshot.Pools, snapshot.Errors)
			}
			if !snapshot.Partial {
				t.Fatal("fallback from incomplete JSON did not mark snapshot partial")
			}
			found := false
			for _, sourceErr := range snapshot.Errors {
				if sourceErr.Source == test.wantErrorSource && sourceErr.Partial {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s fallback error: %+v", test.wantErrorSource, snapshot.Errors)
			}
		})
	}
}

func TestSuccessfulEmptyJSONNeedsConsistentNoObjectFallback(t *testing.T) {
	noPools := Result{Stderr: []byte("no pools available\n"), ExitCode: 1}
	noDatasets := Result{Stderr: []byte("no datasets available\n"), ExitCode: 1}
	runner := &fakeRunner{responses: map[string]Result{
		commandKey("zpool", "status", "-j", "--json-int", "-P", "-t"):                                                        {Stdout: []byte(`{"pools":{}}`)},
		commandKey("zpool", "status", "-P", "-p", "-t"):                                                                      noPools,
		commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"): {Stdout: []byte(`{"pools":{}}`)},
		commandKey("zpool", "list", "-H", "-p", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"):         noPools,
		commandKey("zpool", "get", "-j", "--json-int", "all"):                                                                {Stdout: []byte(`{"pools":{}}`)},
		commandKey("zpool", "get", "-H", "-p", "-o", "name,property,value,source", "all"):                                    noPools,
		commandKey("zpool", "iostat", "-H", "-p"):                                                                            noPools,
		commandKey("zfs", "get", "-j", "--json-int", "all"):                                                                  {Stdout: []byte(`{"datasets":{}}`)},
		commandKey("zfs", "get", "-H", "-p", "-o", "name,property,value,source", "all"):                                      noDatasets,
	}}
	snapshot := domain.Snapshot{Errors: []domain.SourceError{}, Findings: []domain.Finding{}, Datasets: []domain.Dataset{}}
	state := collectionState{collector: New(runner), options: (Options{}).withDefaults(), snapshot: &snapshot}
	state.collectZFS(context.Background())
	state.finish()

	if snapshot.Partial || len(snapshot.Errors) != 0 || len(snapshot.Pools) != 0 || len(snapshot.Datasets) != 0 {
		t.Fatalf("consistent no-object host was not accepted cleanly: %+v", snapshot)
	}
}

func TestCollectSysfsHoldersMarksDiskAndChildHoldersActive(t *testing.T) {
	filesystem := fakeFileSystem{directories: map[string][]fs.DirEntry{
		"/sys/class/block":              {fakeDirEntry("sda"), fakeDirEntry("sda1")},
		"/sys/class/block/sda/holders":  {fakeDirEntry("dm-0")},
		"/sys/class/block/sda1/holders": {fakeDirEntry("md127")},
	}}
	drives := []domain.Drive{{DevNode: "/dev/sda", BlockNodes: []string{"sda", "sda1"}}}
	snapshot := domain.Snapshot{Errors: []domain.SourceError{}}
	state := collectionState{collector: New(&fakeRunner{}, WithFileSystem(filesystem)), snapshot: &snapshot}

	state.collectSysfsHolders(drives)

	if !drives[0].InUse {
		t.Fatal("sysfs holders did not mark the drive active")
	}
	for _, want := range []string{"/dev/sda is held by dm-0 (sysfs)", "/dev/sda1 is held by md127 (sysfs)"} {
		if !strings.Contains(drives[0].Use, want) {
			t.Errorf("Drive.Use missing %q: %q", want, drives[0].Use)
		}
	}
	if len(snapshot.Errors) != 0 {
		t.Fatalf("readable holder directories produced errors: %+v", snapshot.Errors)
	}
}

func TestCollectSysfsHoldersUnreadableIsPartial(t *testing.T) {
	filesystem := fakeFileSystem{directories: map[string][]fs.DirEntry{
		"/sys/class/block": {fakeDirEntry("sda")},
	}}
	drives := []domain.Drive{{DevNode: "/dev/sda", BlockNodes: []string{"sda"}}}
	snapshot := domain.Snapshot{Errors: []domain.SourceError{}, Findings: []domain.Finding{}}
	state := collectionState{
		collector: New(&fakeRunner{}, WithFileSystem(filesystem)),
		options:   (Options{}).withDefaults(),
		snapshot:  &snapshot,
	}

	state.collectSysfsHolders(drives)
	state.finish()

	if !snapshot.Partial {
		t.Fatal("unreadable holders directory did not make snapshot partial")
	}
	found := false
	for _, sourceErr := range snapshot.Errors {
		if sourceErr.Source == "drive.holders" && sourceErr.Partial && strings.Contains(sourceErr.Message, "/sys/class/block/sda/holders") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing partial drive.holders source error: %+v", snapshot.Errors)
	}
}

func TestCollectorMissingToolsReturnsPartialSnapshot(t *testing.T) {
	fixedTime := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	runner := &fakeRunner{responses: make(map[string]Result), defaultResult: Result{ExitCode: -1, Err: exec.ErrNotFound}}
	filesystem := fakeFileSystem{files: map[string][]byte{
		"/etc/machine-id":              []byte("machine-123\n"),
		"/etc/os-release":              []byte("PRETTY_NAME=\"Test Linux\"\n"),
		"/proc/sys/kernel/osrelease":   []byte("6.0-test\n"),
		"/proc/self/mountinfo":         []byte("42 35 8:2 / / rw,relatime - ext4 /dev/sda2 rw\n"),
		"/proc/spl/kstat/zfs/arcstats": fixture(t, "arcstats.txt"),
	}}
	snapshot, err := New(runner, WithFileSystem(filesystem), WithClock(func() time.Time { return fixedTime })).Collect(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Partial || len(snapshot.Errors) < 6 {
		t.Fatalf("missing tools should yield partial errors, got partial=%v errors=%+v", snapshot.Partial, snapshot.Errors)
	}
	if snapshot.APIVersion == "" || snapshot.Host.MachineID != "machine-123" || snapshot.ARC.Hits != 800 {
		t.Fatalf("available sources should survive missing tools: %+v", snapshot)
	}
	foundPartial := false
	for _, finding := range snapshot.Findings {
		if finding.ID == "collection:partial" {
			foundPartial = true
		}
	}
	if !foundPartial {
		t.Fatalf("missing partial-snapshot finding: %+v", snapshot.Findings)
	}
	for _, call := range runner.calls {
		if call.Capability != CapabilityObserve {
			t.Errorf("non-observe capability: %+v", call)
		}
	}
}
