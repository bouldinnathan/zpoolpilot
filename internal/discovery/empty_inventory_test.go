// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"context"
	"errors"
	"testing"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func emptyOpenZFS24Responses() map[string]Result {
	return map[string]Result{
		commandKey("zpool", "status", "-j", "--json-int", "-P", "-t"):                                                        {Stdout: []byte(`{"output_version":{"command":"zpool status","vers_major":0,"vers_minor":1},"pools":{}}`)},
		commandKey("zpool", "status", "-P", "-p", "-t"):                                                                      {Stderr: []byte("no pools available\n")},
		commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"): {},
		commandKey("zpool", "list", "-H", "-p", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health"):         {},
		commandKey("zpool", "get", "-j", "--json-int", "all"):                                                                {Stdout: []byte(`{"output_version":{"command":"zpool get","vers_major":0,"vers_minor":1},"pools":{}}`)},
		commandKey("zpool", "get", "-H", "-p", "-o", "name,property,value,source", "all"):                                    {},
		commandKey("zpool", "iostat", "-H", "-p"):                                                                            {},
		commandKey("zfs", "get", "-j", "--json-int", "all"):                                                                  {Stdout: []byte(`{"output_version":{"command":"zfs get","vers_major":0,"vers_minor":1},"datasets":{}}`)},
		commandKey("zfs", "get", "-H", "-p", "-o", "name,property,value,source", "all"):                                      {},
	}
}

func TestOpenZFS24EmptyInventoryRequiresIndependentAgreement(t *testing.T) {
	statusKey := commandKey("zpool", "status", "-P", "-p", "-t")
	listKey := commandKey("zpool", "list", "-j", "--json-int", "-o", "name,guid,size,allocated,free,fragmentation,capacity,health")
	getKey := commandKey("zpool", "get", "-H", "-p", "-o", "name,property,value,source", "all")
	datasetKey := commandKey("zfs", "get", "-H", "-p", "-o", "name,property,value,source", "all")
	for _, test := range []struct {
		name    string
		change  func(map[string]Result)
		partial bool
	}{
		{"confirmed empty host", func(map[string]Result) {}, false},
		{"all sources blank is insufficient", func(r map[string]Result) { r[statusKey] = Result{} }, true},
		{"status warning is insufficient", func(r map[string]Result) {
			r[statusKey] = Result{Stderr: []byte("no pools available\npermission denied\n")}
		}, true},
		{"interrupted list", func(r map[string]Result) { r[listKey] = Result{TimedOut: true} }, true},
		{"get failure", func(r map[string]Result) { r[getKey] = Result{ExitCode: 1, Err: errors.New("failed")} }, true},
		{"dataset warning", func(r map[string]Result) { r[datasetKey] = Result{Stderr: []byte("permission denied\n")} }, true},
		{"contradictory pool", func(r map[string]Result) { r[listKey] = Result{Stdout: fixture(t, "zpool_list.json")} }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := emptyOpenZFS24Responses()
			test.change(responses)
			snapshot := domain.Snapshot{}
			state := collectionState{collector: New(&fakeRunner{responses: responses}), options: (Options{}).withDefaults(), snapshot: &snapshot}
			state.collectZFS(context.Background())
			state.finish()
			if snapshot.Partial != test.partial {
				t.Fatalf("partial=%v want=%v errors=%+v", snapshot.Partial, test.partial, snapshot.Errors)
			}
			if !test.partial && (len(snapshot.Errors) != 0 || len(snapshot.Pools) != 0 || len(snapshot.Datasets) != 0) {
				t.Fatalf("empty inventory was not clean: %+v", snapshot)
			}
		})
	}
}
