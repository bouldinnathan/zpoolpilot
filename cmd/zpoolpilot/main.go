// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bouldinnathan/zpoolpilot/internal/config"
)

var (
	version = "0.1.0-alpha.1"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:]))
}

type globalOptions struct {
	configPath  string
	readOnly    bool
	colorMode   string
	showVersion bool
}

type app struct {
	cfg        config.Config
	configPath string
	readOnly   bool
	color      bool
	in         *os.File
	out        *os.File
	errOut     *os.File
	reader     *bufio.Reader
}

func run(ctx context.Context, argv []string) int {
	global, args, err := parseGlobals(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zpoolpilot:", terminalText(err.Error()))
		return 64
	}
	if global.showVersion {
		fmt.Printf("zpoolpilot %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}
	loadConfig := config.Load
	if os.Geteuid() == 0 {
		loadConfig = config.LoadPrivileged
	}
	cfg, err := loadConfig(global.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zpoolpilot: load config:", terminalText(err.Error()))
		return 78
	}
	a := &app{cfg: cfg, configPath: global.configPath, readOnly: global.readOnly, color: colorEnabled(global.colorMode, os.Stdout), in: os.Stdin, out: os.Stdout, errOut: os.Stderr, reader: bufio.NewReader(os.Stdin)}
	if len(args) == 0 {
		args = []string{"tui"}
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		a.usage(args[1:])
		return 0
	}
	if topic, ok := inlineHelpTopic(args); ok {
		a.usage(topic)
		return 0
	}
	if global.readOnly && isMutationCommand(args) {
		fmt.Fprintln(os.Stderr, "zpoolpilot: command is unavailable in --read-only mode")
		return 77
	}
	code, err := a.dispatch(ctx, args)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, "zpoolpilot: cancelled")
			return 130
		}
		fmt.Fprintln(os.Stderr, "zpoolpilot:", terminalText(err.Error()))
		if code == 0 {
			code = 1
		}
	}
	return code
}

func inlineHelpTopic(args []string) ([]string, bool) {
	for index, argument := range args {
		if index > 0 && (argument == "--help" || argument == "-h") {
			return append([]string(nil), args[:index]...), true
		}
	}
	return nil, false
}

func defaultConfigPath() string {
	if value := os.Getenv("ZPOOLPILOT_CONFIG"); value != "" {
		return value
	}
	return "/etc/zpoolpilot/config.toml"
}

func parseGlobals(argv []string) (globalOptions, []string, error) {
	o := globalOptions{configPath: defaultConfigPath(), colorMode: "auto"}
	rest := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--read-only":
			o.readOnly = true
		case a == "--version" || a == "-V":
			o.showVersion = true
		case a == "--config":
			if i+1 >= len(argv) {
				return o, nil, errors.New("--config requires a path")
			}
			i++
			o.configPath = argv[i]
		case strings.HasPrefix(a, "--config="):
			o.configPath = strings.TrimPrefix(a, "--config=")
		case a == "--color":
			if i+1 >= len(argv) {
				return o, nil, errors.New("--color requires auto, always, or never")
			}
			i++
			o.colorMode = argv[i]
		case strings.HasPrefix(a, "--color="):
			o.colorMode = strings.TrimPrefix(a, "--color=")
		default:
			rest = append(rest, a)
		}
	}
	if o.colorMode != "auto" && o.colorMode != "always" && o.colorMode != "never" {
		return o, nil, fmt.Errorf("invalid --color value %q", o.colorMode)
	}
	return o, rest, nil
}

func colorEnabled(mode string, file *os.File) bool {
	if mode == "never" || os.Getenv("NO_COLOR") != "" {
		return false
	}
	if mode == "always" {
		return true
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
}

func isMutationCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "plan", "apply", "admin":
		return true
	case "inventory":
		return len(args) > 1 && (args[1] == "map" || args[1] == "verify" || args[1] == "unmap" || args[1] == "locate")
	case "monitor":
		for _, a := range args[1:] {
			if a == "--wake" {
				return true
			}
		}
	}
	return false
}

func (a *app) dispatch(ctx context.Context, args []string) (int, error) {
	switch args[0] {
	case "tui":
		return a.runTUI(ctx, args[1:])
	case "status":
		return a.runStatus(ctx, args[1:])
	case "monitor":
		return a.runMonitor(ctx, args[1:])
	case "inventory":
		return a.runInventory(ctx, args[1:])
	case "plan":
		return a.runPlan(ctx, args[1:])
	case "apply":
		return a.runApply(ctx, args[1:])
	case "doctor":
		return a.runDoctor(ctx, args[1:])
	default:
		return 64, fmt.Errorf("unknown command %q; run `zpoolpilot help`", args[0])
	}
}

func (a *app) usage(topic []string) {
	if len(topic) > 0 {
		switch topic[0] {
		case "plan":
			a.planUsage(topic[1:])
			return
		case "inventory":
			fmt.Fprint(a.out, `Usage:
  zpoolpilot inventory scan [--json] [--wake]
  zpoolpilot inventory list [--json]
  zpoolpilot inventory mappings [--json]
  zpoolpilot inventory map --drive ID --bay LABEL [--enclosure NAME] [--slot N]
  zpoolpilot inventory verify --drive ID [--enclosure NAME] [--bay LABEL] [--slot N]
  zpoolpilot inventory unmap --drive EXACT_STORED_ID
  zpoolpilot inventory locate --drive ID --state on|off

Use scan without --wake for the safe default. map records an inferred bay;
verify records a physically checked mapping. mappings includes absent drives;
unmap retires one exact stored key after a yellow root/TTY confirmation.
locate --state on requires a present, strongly identified drive and a physical
verification no older than 15 minutes. locate --state off accepts the exact
stored key even when its drive is absent or verification is older; clear an
active identify LED before unmapping its record.
`)
			return
		case "monitor":
			fmt.Fprint(a.out, `Usage:
  zpoolpilot --read-only monitor collect --profile fast|hardware|all [--wait] [--include-commands]
  zpoolpilot --read-only monitor serve [--listen HOST:PORT] [--max-age DURATION]

The exporter serves the cached snapshot and never polls disks during a scrape.
`)
			return
		case "apply":
			fmt.Fprint(a.out, "Usage: sudo zpoolpilot apply [--command-timeout DURATION] PLAN.json\n\nApply requires root, a real terminal, an unexpired plan, fresh rediscovery, and the displayed confirmations.\n")
			return
		case "status":
			fmt.Fprint(a.out, "Usage: zpoolpilot --read-only status [--json] [--refresh] [--include-commands]\n")
			return
		}
	}
	fmt.Fprint(a.out, `ZpoolPilot ALPHA - safety-focused local OpenZFS operations and monitoring

Usage:
  zpoolpilot [--config PATH] [--read-only] [--color auto|always|never] COMMAND

Commands:
  tui                         Full-screen dashboard (default)
  status [--json] [--refresh] [--include-commands]
                              Read current pool, dataset, drive, and cache health
  inventory scan|list         Discover or list present drives and bay positions
  inventory mappings          List every authoritative mapping, including absent drives
  inventory map|verify|unmap  Create, verify, or retire a manual drive bay mapping
  inventory locate            Show or control a supported bay identify LED
  plan OPERATION              Create an expiring, inspectable action plan
  apply PLAN.json             Revalidate and interactively apply a plan as root
  monitor collect|serve       Refresh the snapshot cache or serve Prometheus
  doctor                      Check runtime capabilities and safety dependencies

Plan operations:
  create-pool, prepare-disk, replace-disk, attach-mirror,
  expand-raidz, add-vdev, add-spare, replace-boot-disk,
  scrub-start, scrub-stop, trim-start, trim-stop, clear-errors

Global options may appear before or after the command. Use "zpoolpilot help plan"
and README.md for examples. Every storage-changing command remains interactive.
`)
}

func (a *app) planUsage(topic []string) {
	if len(topic) == 0 {
		fmt.Fprint(a.out, `Usage: zpoolpilot plan OPERATION [flags]

Operations:
  create-pool       --pool NAME --topology TYPE --drives ID[,ID...]
  prepare-disk      --drive ID
  replace-disk      --pool POOL --vdev OLD --drive NEW
  attach-mirror     --pool POOL --vdev EXISTING --drive NEW
  expand-raidz      --pool POOL --vdev RAIDZ --drive NEW
  add-vdev          --pool POOL --topology TYPE --drives ID[,ID...]
  add-spare         --pool POOL --drives ID[,ID...]
  replace-boot-disk --pool POOL --vdev OLD --source-drive HEALTHY --drive NEW --boot-mode systemd-boot|grub
  scrub-start|scrub-stop|trim-start|trim-stop|clear-errors --pool POOL

Every plan is a short-lived preview. Applying it is a separate, interactive,
root-only step. Run "zpoolpilot plan OPERATION --help" for an operation synopsis.
`)
		return
	}
	syntax := map[string]string{
		"create-pool":       "zpoolpilot plan create-pool --pool NAME --topology single|mirror|raidz1|raidz2|raidz3 --drives ID[,ID...] [--profile general|proxmox] [--property K=V] [--dataset-property K=V] [--encrypt off|passphrase|keyfile] [--keyfile PATH] [--key-format passphrase|raw|hex] [--expert] [--out PATH]",
		"prepare-disk":      "zpoolpilot plan prepare-disk --drive ID [--out PATH]",
		"replace-disk":      "zpoolpilot plan replace-disk --pool POOL --vdev OLD --drive NEW [--out PATH]",
		"attach-mirror":     "zpoolpilot plan attach-mirror --pool POOL --vdev EXISTING --drive NEW [--out PATH]",
		"expand-raidz":      "zpoolpilot plan expand-raidz --pool POOL --vdev RAIDZ --drive NEW [--out PATH]",
		"add-vdev":          "zpoolpilot plan add-vdev --pool POOL --topology TYPE --drives ID[,ID...] [--out PATH]",
		"add-spare":         "zpoolpilot plan add-spare --pool POOL --drives ID[,ID...] [--out PATH]",
		"replace-boot-disk": "zpoolpilot plan replace-boot-disk --pool POOL --vdev OLD --source-drive HEALTHY --drive NEW --boot-mode systemd-boot|grub [--out PATH]",
		"scrub-start":       "zpoolpilot plan scrub-start --pool POOL [--out PATH]",
		"scrub-stop":        "zpoolpilot plan scrub-stop --pool POOL [--out PATH]",
		"trim-start":        "zpoolpilot plan trim-start --pool POOL [--out PATH]",
		"trim-stop":         "zpoolpilot plan trim-stop --pool POOL [--out PATH]",
		"clear-errors":      "zpoolpilot plan clear-errors --pool POOL [--out PATH]",
	}
	operation := topic[0]
	if value, present := syntax[operation]; present {
		fmt.Fprintf(a.out, "Usage: %s\n\nPlanning performs fresh, no-wake discovery before selecting any device.\n", value)
		return
	}
	fmt.Fprintf(a.out, "Unknown plan operation %q. Run `zpoolpilot plan --help`.\n", terminalText(operation))
}
