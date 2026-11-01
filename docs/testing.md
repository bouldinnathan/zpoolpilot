# Testing and destructive-test boundary

The normal test path is fully offline and never needs ZFS, SMART access, root,
or real block devices:

```sh
make check
make test-race
```

`make check` verifies formatting, runs `go vet`, disables module and toolchain
downloads while running every unit test, validates packaging policy, builds a
staged install, and checks that a pre-existing administrator configuration is
preserved. It also compiles the guarded integration test binary and removes it
without running it (`make test-integration-build`). Formatting parse failures
fail the check rather than being suppressed. The default GitHub Actions workflow runs this path with
`GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`, and `-mod=readonly`.

Tests use captured OpenZFS, `lsblk`, udev, SMART, and ARC fixtures plus injected
command runners. Parser fixtures must include healthy and degraded pools,
mirrors, RAIDZ/dRAID, special/log/cache/spare classes, missing fields, partial
output, per-leaf and mixed/unsupported TRIM states, legacy tabular output,
duplicate or absent disk identifiers, sleeping drives, and ATA/SAS/NVMe SMART
shapes. SMART guard regressions exercise unsupported ATA power checks with no retry,
skipped SAS/SCSI/USB/unknown transports, explicit ATA/SAT/NVMe types,
and object-shaped sleeping states. The complete mixed-transport fixture uses
an explicitly waking collection so SAS parsing remains covered.

A nonzero `smartctl` status must be tested as a bitmask carrying
possible health findings, not automatically as a failed process.

Wear fixtures cover authoritative NVMe percentage-used data, allowlisted ATA
remaining-life and used-life names, separator/case normalization, 0-100 range
checks, and the rule that raw or disagreeing ATA candidates remain unknown.

Inventory tests distinguish tentative manual/inferred map records from
timestamped verify promotion, exercise immediate overlay on cached snapshots,
and clear stale manual/conflict state before trusted mappings are applied.
File-store tests cover schema round trips, safe output permissions,
symlink/writable-file rejection, duplicate stable enclosure slots, atomic
expected-record deletion, and reuse of a retired absent drive's bay by its
replacement. CLI tests require exact stored unmap keys, deterministic
all-mapping output, and read-only mutation classification. CLI and SES tests
also cover root/TTY authority, known-slot and 15-minute freshness gates,
canonical character-device paths, type-13 validation, and reliable udev
WWN/serial identity parsing. Locate tests keep present-drive, strong-identity,
and recent-verification gates for LED-on while allowing LED-off through an
exact trusted key for an absent drive or older verification; inferred,
future-dated, and non-SES records remain refused. The locate drive-identity
check is tested against stable-hardware and major:minor changes, and rejected
bay-store trust is tested as explicit partial status.

Fail-closed discovery regressions cover malformed/non-object pools and vdevs,
missing pool/vdev identity or state, missing leaf paths, unknown scan shapes,
strict LSBLK mountpoint/filesystem/child structure, and sysfs holders on both a
disk and its descendants. Cross-source tests require exact bidirectional pool
name-to-GUID equality among status/list/get and prove that truncated JSON cannot
erase bootfs or a dataset mounted at `/`. A clean no-pools host needs
independently consistent no-object evidence. Mountinfo fixtures prove that an
actual ZFS `/` remains boot-critical with `bootfs=none` and
`mountpoint=legacy`, while missing, malformed, duplicate, or unmatched root
evidence fails closed.

Planner tests cover every typed lifecycle and routine-maintenance operation,
plan expiry/tampering, exact argv reconstruction, confirmation risk, fresh
identity/topology revalidation before each command, audit failures, and lock
contention. They also cover exact 32-lowercase-hex plan IDs, serialized
`DriveRef` capacity/sector-geometry/major:minor pins, raw ownership-signature
blockers, symmetric scrub/manual-TRIM coordination, active or unknown
scan/TRIM authority, authoritative selected-vdev/RAIDZ-child sizes at plan and
apply, largest-data-leaf sizing for every spare, and target-health
block/warning/revalidation behavior. Disk-guard tests use fake filesystem state
to verify last-mile by-id, block-device, major:minor, sysfs, and Proxmox
partition-parent checks. Proxmox boot tests use synthetic snapshots and fake
runners; they verify the fixed partition/ESP command sequence and blockers
without touching a boot disk.

Terminal tests use a pseudo-terminal to exercise raw keys, resize redraws,
signal handling, and terminal restoration. Adversarial snapshot and refresh
errors verify that plain output contains no terminal controls and colored
output contains only renderer-owned ANSI sequences.

Monitoring tests cover Prometheus metric/label escaping, bounded cache loading,
readiness age, collection-lock exclusion/symlink defense, SMART carry-forward
only across an exact identity match, sleeping-drive retention, and
stale/never-collected findings. Snapshot and SMART timestamps more than one
minute in the future fail cache loading/readiness; a future SMART time produces
an unknown finding during freshness evaluation. JSON and database fallback
tests also reject an unsupported snapshot API version. Runner/store tests prove
bounded stdout/stderr capture and fail-closed overflow. Privileged-config,
audit-file, keyfile, lock, and shared-state tests exercise ownership, symlink,
link-count, mode, and fixed-subtree checks. The CLI contract reserves health
states 0-3, collector incompletion 69, and persistence failure 74.
Fresh partial snapshots also fail readiness. Exact ARC, pool-list, pool-status,
and pool-I/O source errors are tested to omit their numeric samples, while a
reported JSON-probe failure followed by a successful legacy fallback retains
the recovered measurements.

## Compatibility contracts

OpenZFS 2.3 is the v1 minimum for the preferred versioned `zpool status` JSON
interface. The official [2.2 `zpool-status(8)` manual](https://openzfs.github.io/openzfs-docs/man/v2.2/8/zpool-status.8.html)
does not list `-j`; the official [2.3 manual](https://openzfs.github.io/openzfs-docs/man/v2.3/8/zpool-status.8.html)
does. Legacy tabular parsing remains covered by fixtures as a best-effort
compatibility path, but it must never turn a parse failure into a healthy
snapshot.

OpenZFS documents native dry-run for [`zpool create -n`](https://openzfs.github.io/openzfs-docs/man/master/8/zpool-create.8.html)
and [`zpool add -n`](https://openzfs.github.io/openzfs-docs/man/master/8/zpool-add.8.html),
but v1 uses deterministic application-generated previews for every operation.
[`zpool replace`](https://openzfs.github.io/openzfs-docs/man/master/8/zpool-replace.8.html)
has no dry-run option. Exact argv reconstruction and apply-time
identity/topology revalidation are therefore mandatory rather than treating a
preview as an execution guarantee.

OpenZFS defines `zpool trim -s` as suspend, not cancel, and documents that a
plain `zpool trim` resumes it. Maintenance plans and TRIM-state fixtures enforce
that distinction; see [`zpool-trim(8)`](https://openzfs.github.io/openzfs-docs/man/master/8/zpool-trim.8.html).

The upstream [`smartctl(8)` source manual](https://github.com/smartmontools/smartmontools/blob/main/src/smartctl.8.in)
defines its eight-bit exit status. Fixture and live tests must preserve valid
JSON even when health/history bits make that status nonzero.

## Packaging and service checks

`scripts/check-packaging.sh` checks the service privilege split and required
hardening directives. When usable in the host environment it also runs
`systemd-analyze verify` and offline security analysis with a maximum exposure
threshold of 7.0 (`--threshold=70`, in tenths). The isolated root includes
minimal fixtures for external systemd targets; packaged units are verified
without disabling their default dependencies. It validates sysusers
and tmpfiles declarations in a temporary synthetic root so the real machine is
not changed.

`scripts/check-install.sh` stages `make install` below a temporary directory,
checks paths and modes, executes the staged binary's version command, and
reinstalls over a sentinel config to prove local administrator configuration is
not overwritten. Neither check enables or starts services.

## Live ZFS integration suite

### Repeatable disposable Proxmox setup

The local SSH MCP profile must mark the selected guest ephemeral and
shell-enabled. Independently verify it is a disposable nested Proxmox VM with
no valuable guests, imported pools, or shared filesystems before copying this
checkout into a root-owned directory such as `/root/zpoolpilot-alpha`. Do not
use a production Proxmox hypervisor. No agent runtime is installed in the guest.

Inside that confirmed guest, install the test prerequisites with:

```sh
cd /root/zpoolpilot-alpha
ZPOOLPILOT_LAB_SETUP=I_CONFIRM_DISPOSABLE_PROXMOX_VM \
  ./scripts/setup-proxmox-lab.sh
make check
make test-race
ZPOOLPILOT_DESTRUCTIVE_TESTS=DESTROY_TEMPORARY_ZFS_TEST_POOLS \
ZPOOLPILOT_INTEGRATION_VM=ephemeral make test-integration
```

The setup script rechecks the VM, Proxmox, empty guest/pool inventory, and
absence of shared mounts before installing prerequisites from the guest's
configured Debian distribution sources. It does not configure rollback;
reboot rollback must already be provided by the disposable VM environment.
After reboot, recheck the SSH profile and guest identity, copy the current
checkout again, and rerun the same setup. Preserve logs locally before reset.
Verify a pre-reset sentinel file, installed toolchain, and test checkout are
absent after reboot before recording rollback as confirmed.

### Guarded loop-backed suite

Execution of the live suite is deliberately excluded from `make`, `make check`,
normal CI, and scheduled CI. Compilation alone is part of the offline check.
The repository provides this guarded harness. Ordinary unit-test and release
check results do not imply that it ran; live runs are recorded separately below.
Run it only on a disposable, ephemeral Linux virtual
machine that has no valuable pools and provides OpenZFS 2.3+, `losetup`, and
root:

```sh
sudo env \
  PATH="$PATH" \
  ZPOOLPILOT_DESTRUCTIVE_TESTS=DESTROY_TEMPORARY_ZFS_TEST_POOLS \
  ZPOOLPILOT_INTEGRATION_VM=ephemeral \
  make test-integration
```

The harness refuses to start unless all of these conditions hold:

- effective UID is zero;
- both acknowledgement variables exactly match their documented values;
- `systemd-detect-virt --vm` identifies a VM and no container is detected;
- every vdev resolves to a loop device whose backing file is inside the newly
  created integration directory;
- the pool name matches a generated `zpoolpilot_ci_<numbers>` pattern and did not
  exist before the run.

Because the tested CLI runs as root, each run puts its configuration and bay
mapping file in a unique trusted directory below `/etc/zpoolpilot` and its
temporary cache/state, runtime, and audit files in unique confined directories
below `/var/lib/zpoolpilot`, `/run/zpoolpilot`, and `/var/log/zpoolpilot`. Cleanup
names those files and directories explicitly.

It creates sparse backing files, attaches dedicated loop devices, creates a
uniquely named mirror with `cachefile=none` and no mountpoint, adds a spare,
replaces one mirror member, and verifies the resulting pool through the built
CLI. Before attaching any loop device, it asks the built CLI to validate the
generated, confined integration configuration. It also confirms that
`--read-only` rejects apply and that all collected
command traces are observer operations. Cleanup checks the recorded pool GUID
before destroying the pool, detaches only loop devices whose backing files are
still inside the run directory, and removes only explicitly recorded files.
If creation was attempted but the pool GUID or successful destruction cannot
be established, cleanup preserves its loop devices and backing files. The
workflow's final pool-list check fails on command errors instead of treating
them as proof that no test pool remains.

The manual-only `.github/workflows/zfs-integration.yml` additionally requires a
self-hosted runner labeled `zpoolpilot-zfs-vm` and the protected
`zpoolpilot-destructive-tests` environment. That runner should be destroyed after
each job: no in-process trap can guarantee cleanup after a VM crash, kernel
panic, or forced runner termination.

This suite does not execute the Proxmox boot-disk replacement, reboot a host,
wait for a real resilver, or validate physical SES slot numbering, identify
LEDs, controller-specific SMART permissions, hot removal, or real-disk
firmware. Those require a separate opt-in hardware lab/throwaway Proxmox VM
with representative HBAs and enclosures; manual bay verification remains the
safe fallback.

## Recorded alpha validation — 2026-11-01

The local tree passed `make check`, `make test-race`, and the alpha build.
A negative packaging check also confirmed that a missing required systemd
unit is rejected rather than hidden by the synthetic target fixtures.

The SSH MCP profile `pve-test` was explicitly ephemeral and shell-enabled.
Read-only inspection confirmed a nested KVM guest running Proxmox VE 9.2.2
(kernel 7.0.2-6-pve), an empty VM/container inventory, no imported ZFS pools,
and no shared filesystem mounts before setup. The guest used OpenZFS
2.4.2-pve1 and Debian Go 1.24.4.

| Check in disposable guest | Result |
| --- | --- |
| `make check` | Passed |
| `make test-race` | Passed |
| Guarded `make test-integration` | Passed, including a rerun after the empty-inventory fix |
| Packaged fast and hardware collectors | Started successfully under their configured users |
| Exporter `/healthz` and `/metrics` | Responded successfully |
| Exporter `/readyz` with unsupported virtual-disk SMART | Correctly returned 503 for partial telemetry |
| Normal exporter stop | Clean inactive service, `Result=success` |
| Integration cleanup | No test pool or associated loop backing remained |

The live test created a mirror, added a spare, replaced a member and waited
for resilvering using the guarded harness's OpenZFS commands. The Go tests
verified the built CLI's collection and read-only apply rejection. This run
does **not** validate execution of application-generated destructive plans,
Proxmox boot-disk replacement, a ZFS-root boot/reboot, real SMART standby,
physical bay/SES LEDs, controllers, or disk firmware.

The service smoke test used the installed files and actual systemd isolation:

```sh
make install
systemd-sysusers /usr/lib/sysusers.d/zpoolpilot.conf
systemd-tmpfiles --create /usr/lib/tmpfiles.d/zpoolpilot.conf
systemctl daemon-reload
systemctl start zpoolpilot-collect-hardware.service
systemctl start zpoolpilot-collect-fast.service
systemctl start zpoolpilot-exporter.service
curl --retry 5 --retry-connrefused --retry-delay 1 http://127.0.0.1:9815/healthz
curl http://127.0.0.1:9815/metrics
curl -i http://127.0.0.1:9815/readyz
systemctl stop zpoolpilot-exporter.service
systemctl show -p ActiveState -p Result zpoolpilot-exporter.service
```

No service timers were enabled. Unsupported QEMU disk SMART remained unknown;
the namespace-restricted optional Proxmox boot probe also reported its denied
`unshare` operation without granting extra service privileges. Empty-inventory
ZFS sources no longer produced parse errors after the fix. Raw guest logs and
environment evidence were copied to the local machine before reboot.

### Reboot/reset result

A subsequent guest reboot changed the kernel boot ID but **preserved** the
checkout, installed Go toolchain, ZpoolPilot binary, configuration, state, and
an explicit sentinel file. Automatic reboot rollback is therefore **not
verified** for this lab, despite its ephemeral SSH profile flag. No further
destructive test changes were made after that observation. A separate externally
configured snapshot reset must be confirmed before relying on a clean baseline;
this project does not reconfigure the parent hypervisor or claim that reboot
alone restores it. The setup script remains repeatable after a confirmed reset.
