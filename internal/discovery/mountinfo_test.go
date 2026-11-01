// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"context"
	"strings"
	"testing"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func TestParseRootMountInfo(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`41 35 0:31 /with\040space /srv\040data rw,nosuid shared:7 - tmpfs tmpfs rw`,
		`42 35 8:2 /root\040dir\134name / rw,relatime - ext4 /dev/mapper/root\040disk rw`,
	}, "\n") + "\n"
	root, err := parseRootMountInfo([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if root.Root != "/root dir\\name" || root.MountPoint != "/" || root.FSType != "ext4" || root.Source != "/dev/mapper/root disk" {
		t.Fatalf("decoded root mount = %+v", root)
	}

	zfsRoot, err := parseRootMountInfo([]byte(`77 35 0:42 / / rw,relatime shared:1 - zfs tank/ROOT/default rw,xattr,posixacl` + "\n"))
	if err != nil || zfsRoot.FSType != "zfs" || zfsRoot.Source != "tank/ROOT/default" {
		t.Fatalf("ZFS root = %+v, err=%v", zfsRoot, err)
	}
}

func TestParseRootMountInfoRejectsMissingMalformedAndAmbiguousEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing root", input: `41 35 0:31 / /srv rw - tmpfs tmpfs rw`, want: "no mount at /"},
		{name: "missing separator", input: `42 35 8:2 / / rw shared:1 ext4 /dev/sda2 rw`, want: "separator"},
		{name: "bad device", input: `42 35 bad / / rw - ext4 /dev/sda2 rw`, want: "mount device"},
		{name: "bad escape", input: `42 35 8:2 / / rw - ext4 /dev/root\999 rw`, want: "unsupported escape"},
		{name: "duplicate root", input: "42 35 8:2 / / rw - ext4 /dev/sda2 rw\n43 35 8:3 / / rw - ext4 /dev/sda3 rw\n", want: "multiple mounts"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRootMountInfo([]byte(test.input)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseRootMountInfo() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLegacyZFSRootMountMarksExactDiscoveredPoolBootCritical(t *testing.T) {
	t.Parallel()
	poolGet := strings.Replace(string(fixture(t, "zpool_get.json")), `"bootfs":{"value":"tank/ROOT/default"`, `"bootfs":{"value":"none"`, 1)
	zfsGet := strings.Replace(string(fixture(t, "zfs_get.json")), `"mountpoint":{"value":"/","source"`, `"mountpoint":{"value":"legacy","source"`, 1)
	runner := &fakeRunner{responses: map[string]Result{
		commandKey("zpool", "status", "-j", "--json-int", "-P", "-t"):                                                        {Stdout: fixture(t, "zpool_status.json")},
		commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"): {Stdout: fixture(t, "zpool_list.json")},
		commandKey("zpool", "get", "-j", "--json-int", "all"):                                                                {Stdout: []byte(poolGet)},
		commandKey("zpool", "iostat", "-H", "-p"):                                                                            {Stdout: []byte("tank\t1820\t180\t10\t20\t30\t40\n")},
		commandKey("zfs", "get", "-j", "--json-int", "all"):                                                                  {Stdout: []byte(zfsGet)},
	}}
	filesystem := fakeFileSystem{files: map[string][]byte{
		"/test/mountinfo": []byte("77 35 0:42 / / rw,relatime - zfs tank/ROOT/default rw,xattr\n"),
	}}
	snapshot := domain.Snapshot{Errors: []domain.SourceError{}, Findings: []domain.Finding{}, Datasets: []domain.Dataset{}}
	state := collectionState{
		collector: New(runner, WithFileSystem(filesystem)),
		options:   (Options{MountInfoPath: "/test/mountinfo"}).withDefaults(),
		snapshot:  &snapshot,
	}
	state.collectZFS(context.Background())
	if len(snapshot.Pools) != 1 || snapshot.Pools[0].BootCritical {
		t.Fatalf("legacy properties unexpectedly identified boot pool before mountinfo: %+v", snapshot.Pools)
	}
	snapshot.Pools = append(snapshot.Pools, domain.Pool{Name: "backup", GUID: "backup-guid"})
	state.collectRootMount()
	state.finish()
	if len(snapshot.Pools) != 2 || snapshot.Pools[0].Name != "tank" || !snapshot.Pools[0].BootCritical || snapshot.Pools[1].BootCritical {
		t.Fatalf("legacy root pool was not marked boot-critical: %+v", snapshot.Pools)
	}
	if snapshot.Partial {
		t.Fatalf("valid root evidence made snapshot partial: %+v", snapshot.Errors)
	}
}

func TestCollectRootMountAcceptsOrdinaryNonZFSRoot(t *testing.T) {
	t.Parallel()
	filesystem := fakeFileSystem{files: map[string][]byte{
		"/test/mountinfo": []byte("42 35 8:2 / / rw,relatime - ext4 /dev/sda2 rw\n"),
	}}
	snapshot := domain.Snapshot{Pools: []domain.Pool{{Name: "tank", GUID: "pool-guid"}}, Errors: []domain.SourceError{}}
	state := collectionState{collector: New(&fakeRunner{}, WithFileSystem(filesystem)), options: (Options{MountInfoPath: "/test/mountinfo"}).withDefaults(), snapshot: &snapshot}
	state.collectRootMount()
	state.finish()
	if snapshot.Partial || snapshot.Pools[0].BootCritical || len(snapshot.Errors) != 0 {
		t.Fatalf("ordinary root was not accepted cleanly: %+v", snapshot)
	}
}

func TestCollectRootMountFailsClosedOnMissingMalformedOrUnmatchedEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string][]byte
	}{
		{name: "missing", files: map[string][]byte{}},
		{name: "malformed", files: map[string][]byte{"/test/mountinfo": []byte("not mountinfo\n")}},
		{name: "unmatched ZFS pool", files: map[string][]byte{"/test/mountinfo": []byte("77 35 0:42 / / rw - zfs missing/ROOT/default rw\n")}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filesystem := fakeFileSystem{files: test.files}
			snapshot := domain.Snapshot{Pools: []domain.Pool{{Name: "tank", GUID: "pool-guid"}}, Errors: []domain.SourceError{}}
			state := collectionState{collector: New(&fakeRunner{}, WithFileSystem(filesystem)), options: (Options{MountInfoPath: "/test/mountinfo"}).withDefaults(), snapshot: &snapshot}
			state.collectRootMount()
			state.finish()
			if !snapshot.Partial || snapshot.Pools[0].BootCritical {
				t.Fatalf("unsafe root evidence did not fail closed: %+v", snapshot)
			}
			found := false
			for _, sourceError := range snapshot.Errors {
				if sourceError.Source == rootMountSource && sourceError.Partial {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing partial %s error: %+v", rootMountSource, snapshot.Errors)
			}
		})
	}
}

func TestMountInfoPathDefault(t *testing.T) {
	t.Parallel()
	if got := (Options{}).withDefaults().MountInfoPath; got != "/proc/self/mountinfo" {
		t.Fatalf("MountInfoPath = %q", got)
	}
}
