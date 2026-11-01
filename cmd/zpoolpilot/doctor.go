// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"runtime"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/execx"
)

type dependency struct {
	name     string
	required bool
	purpose  string
}

func (a *app) runDoctor(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	jsonOut := fs.Bool("json", false, "emit machine-readable checks")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() != 0 {
		return 64, errors.New("doctor takes no positional arguments")
	}
	deps := []dependency{{"zpool", true, "OpenZFS pool status and lifecycle"}, {"zfs", true, "dataset settings"}, {"lsblk", true, "block topology and holders"}, {"udevadm", true, "stable identity"}, {"smartctl", true, "ATA/SAS/NVMe health"}, {"sqlite3", false, "transactional snapshot cache (atomic JSON fallback exists)"}, {"sgdisk", false, "disk preparation and Proxmox boot replacement"}, {"wipefs", false, "recognized signature removal during disk preparation"}, {"partprobe", false, "boot partition refresh"}, {"sg_ses", false, "verified SES identify LEDs"}, {"pveversion", false, "Proxmox detection"}, {"proxmox-boot-tool", false, "boot-critical pool maintenance"}}
	type check struct {
		Name     string `json:"name"`
		Present  bool   `json:"present"`
		Required bool   `json:"required"`
		Path     string `json:"path,omitempty"`
		Purpose  string `json:"purpose"`
	}
	checks := make([]check, 0, len(deps))
	missing := 0
	for _, d := range deps {
		path, err := execx.ResolveProgram(d.name)
		present := err == nil
		if d.required && !present {
			missing++
		}
		checks = append(checks, check{Name: d.name, Present: present, Required: d.required, Path: path, Purpose: d.purpose})
	}
	linux := runtime.GOOS == "linux"
	checks = append(checks, check{Name: "linux", Present: linux, Required: true, Purpose: "supported operating-system boundary"})
	if !linux {
		missing++
	}
	if *jsonOut {
		return boolCode(missing > 0), writeJSON(a.out, map[string]any{"version": version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "read_only": a.readOnly, "checks": checks})
	}
	fmt.Fprintf(a.out, "zpoolpilot %s  %s/%s  %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	for _, c := range checks {
		state := "optional"
		risk := domain.RiskRead
		if c.Required {
			state = "required"
		}
		mark := "present"
		if !c.Present {
			mark = "MISSING"
		}
		fmt.Fprintf(a.out, "%s %-22s %-8s %-8s %s\n", riskLabel(a.color, risk), c.Name, mark, state, c.Purpose)
	}
	runner := a.newRunner(true)
	for _, command := range []execx.Command{{Program: "zpool", Args: []string{"version", "-j"}, Capability: execx.CapabilityObserve, Risk: domain.RiskRead}, {Program: "proxmox-boot-tool", Args: []string{"status"}, Capability: execx.CapabilityObserve, Risk: domain.RiskRead}} {
		if _, err := execx.ResolveProgram(command.Program); err != nil {
			continue
		}
		result, err := runner.Run(ctx, command)
		fmt.Fprintf(a.out, "%s %s\n", riskLabel(a.color, domain.RiskRead), displayCommand(command.Program, command.Args))
		if len(result.Stdout) > 0 {
			fmt.Fprintln(a.out, indent(terminalText(strings.TrimSpace(string(result.Stdout))), "  "))
		}
		if err != nil {
			fmt.Fprintf(a.out, "  unavailable: %s\n", terminalText(err.Error()))
		}
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(a.errOut, "critical: only Linux is supported")
	}
	if missing > 0 {
		return 2, fmt.Errorf("%d required capability checks failed", missing)
	}
	return 0, nil
}

func boolCode(v bool) int {
	if v {
		return 2
	}
	return 0
}
func indent(s, prefix string) string { return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix) }
