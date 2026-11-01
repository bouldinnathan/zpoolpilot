// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"testing"

	"github.com/bouldinnathan/zpoolpilot/internal/planner"
)

func TestMaintenancePlanOperationCLIMap(t *testing.T) {
	t.Parallel()
	for _, operation := range []planner.Operation{
		planner.OperationScrubStart,
		planner.OperationScrubStop,
		planner.OperationTrimStart,
		planner.OperationTrimStop,
		planner.OperationClearErrors,
	} {
		got, ok := maintenancePlanOperation(string(operation))
		if !ok || got != operation {
			t.Fatalf("maintenancePlanOperation(%q) = %q, %t", operation, got, ok)
		}
	}
	if got, ok := maintenancePlanOperation("scrub;destroy"); ok || got != "" {
		t.Fatalf("unsafe operation mapped as %q, %t", got, ok)
	}
}
