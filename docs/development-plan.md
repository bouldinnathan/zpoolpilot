# Development stages — 0.1.0-alpha.1 (unreleased)

The current milestone is to correct and validate the existing single-host
implementation through offline and disposable-VM validation. Boot-recovery and
hardware testing remain later stages. Passing offline tests does not establish live storage or
boot-recovery compatibility.

## Alpha preparation on 2026-11-01

- Renamed the command, package, configuration paths, services, environment
  variables, and schemas to ZpoolPilot under `github.com/bouldinnathan/zpoolpilot`.
- Added GPL-3.0-or-later licensing and explicit unreleased alpha status.
- Fixed executable script modes and isolated systemd target fixtures; the
  security check uses 70 tenths for the documented maximum score of 7.0.
- SMART selects explicit ATA/SAT or NVMe types; ATA power checks stop when
  unsupported. Other transports remain unknown until an authorized waking
  refresh. Modern object-shaped and legacy string-shaped sleeping states
  are supported.
- Supported clean OpenZFS 2.4 hosts whose empty inventory commands return
  exit-zero blank output, gated by independently confirmed empty status and
  consistent property/dataset sources. Interrupted or contradictory sources
  continue to fail closed.
- Treated the exporter's normal cancellation status as a clean systemd stop.
- Validation results and their limits are recorded in `testing.md`.
  Regenerate earlier development plans/configuration
  for the new names and schema identifiers; automatic migration is not provided.

## Offline review completed on 2026-09-07

The review added regression coverage and corrected these behaviors:

- **Storage plans:** FIFO plan paths fail immediately; disk preparation repeats
  raw-ownership checks before each command; boot partition cloning requires
  matching logical sector sizes; single-disk topology accepts one disk only.
- **Operator confirmation:** cancellation interrupts a pending prompt;
  failed command-preview or prompt output cannot authorize execution; an
  incomplete input line cannot count as approval.
- **Collection and cache:** persisted collectors lock before discovery;
  incomplete collections preserve the previous cache; older apply results
  cannot overwrite newer snapshots; cached command traces require opt-in.
  Fast dashboard refreshes retain hardware data only for matching identities.
- **Telemetry:** NVMe wear above 100% stays visible; corrected SCSI errors are
  distinguished from uncorrected errors; interrupted SMART output, incomplete
  ARC/I/O output, invalid numeric values, and ambiguous partition paths cannot
  create misleading healthy readings.
- **Display and export:** stale health findings are recalculated when reading
  cached data and serving metrics. Failed sources show unknown measurements in
  the dashboard. Published JSON uses arrays and objects for required empty
  collections rather than `null`.
- **Persistence and execution:** cancellation is honored by locks and stores;
  invalid or oversized writes preserve existing cache data; ambiguous inventory
  queries fail; configuration rejects duplicate settings and derived-path
  collisions; noninteractive command timeouts stop child processes too.
- **Offline checks:** formatting errors fail the release check; the future
  integration suite is compiled without execution; harness cleanup preserves
  pool backing when ownership or destruction cannot be established.

The normal verification commands are:

```sh
make check
make test-race
```

These cover formatting, vet, offline unit tests, integration-suite compilation,
packaging declarations, staged installation, configuration preservation, and
race detection. Tests use fixtures, temporary files, local helper subprocesses,
SQLite when installed, and pseudo-terminals. No live ZFS operations run.

## Disposable VM integration

Use the explicit guarded harness described in
[testing.md](testing.md) on a disposable VM. Validate actual OpenZFS output and
loop-backed pool creation, spare addition, and replacement. Separately validate
the supported Proxmox boot layouts, replacement sequence, resilver completion,
ESP state, and a controlled reboot. The current harness does not cover the
boot-replacement sequence.

## Later: representative hardware

Validate controller permissions and SMART behavior, disk standby, physical bay
labels and SES numbering, identify LEDs, hot removal/replacement, and real
firmware behavior. Keep manual bay verification until automatic discovery has
been implemented and validated.

## Separate feature backlog

Automatic SES slot discovery, Proxmox storage registration, multi-host control,
alerts, boot-mode conversion, and advanced auxiliary-vdev lifecycle operations
remain outside the implemented v1 scope. Their design and validation should be
scheduled explicitly; they are not implied by passing the current offline
checks.
