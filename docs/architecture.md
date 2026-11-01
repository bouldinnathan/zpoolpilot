# Architecture

```text
OpenZFS JSON/text + lsblk + sysfs/udev + SMART + ARC + /dev/disk links
                                 |
                                 v
root-owned bays.json overlay -> SnapshotV1 + findings
                          |       |       |
                     TUI/text    JSON   atomic cache -> Prometheus

fresh safety snapshot -> typed planner -> untrusted 15-minute plan JSON
                                                   |
                                   root + TTY + exclusive apply lock
                                                   |
                              rebuild argv -> revalidate -> confirm
                                                   |
                                      execute -> rediscover -> audit
```

## Discovery and normalized state

The domain package owns the versioned snapshot and action-plan wire types.
Discovery adapters convert source-specific data into that model. OpenZFS 2.3
JSON is preferred; legacy tabular output is a best-effort monitoring fallback.
Malformed, unavailable, denied, and timed-out sources create explicit source
errors and partial/unknown findings.

Safety parsing is structural and cross-source. Pool status requires pool and
non-root-vdev GUID/state, leaf paths, and recognized scan authority. Pool
status/list/get must have identical bidirectional name-to-GUID coverage, while
zpool bootfs and ZFS root-mount dataset evidence must remain covered. A
syntactically valid empty or truncated JSON object cannot erase a real pool or
its boot-critical classification; only independently consistent no-object
evidence is accepted as an empty host.

Discovery separately parses the actual `/` entry in `/proc/self/mountinfo`.
When its filesystem is ZFS, the dataset source marks the exactly matching pool
boot-critical even if `bootfs=none` and the dataset property is
`mountpoint=legacy`. Missing, malformed, duplicate, or unmatched root-mount
evidence makes safety discovery partial and therefore prevents planning.

Drive discovery combines `lsblk`, udev, sysfs, `/dev/disk/by-id`, and
`/dev/disk/by-path`. The complete LSBLK tree must retain typed mountpoint and
filesystem fields. Holders are collected from sysfs for the disk and every
LSBLK descendant; missing holder authority makes safety discovery partial.
Vdev leaves are correlated back to stable drive IDs.
By-path values provide only inferred placement. The inventory layer merges
manual mappings from the separately authenticated, root-owned `bays.json`,
keyed by stable drive identity, over cached discovery and detects two present
drives assigned to the same bay. `map` records inferred confidence; `verify`
promotes the supplied or saved values with a timestamp and stable enclosure
identity. SES locate accepts only that verified state and revalidates the live
enclosure identity before and after confirmation. Turning the LED on requires
verification no more than 15 minutes old and repeats fresh drive-identity
discovery after the prompt. Turning it off uses the exact stored key and may
clear an orphaned indicator after the drive is absent or verification is older;
both states reject implausibly future-dated records and reload/compare the
authoritative mapping. Read-only `mappings` lists authoritative keys for both present and
absent drives. Root/TTY `unmap` retires one exact stored key so a replacement
can reuse the bay; after confirmation it reloads the record and performs an
atomic compare-and-delete under the writer lock. Operators clear the LED before
unmap because deletion removes its slot authority. Map, verify, unmap, and locate
all require yellow confirmation. Map and verify always require fresh fast
no-wake safety discovery; locate requires it only when turning the LED on. Map
and verify repeat drive fingerprint/major:minor discovery after confirmation;
verify also repeats its enclosure probe before persisting.

Pool/vdev state, raw capacity, allocator fragmentation, scan progress, manual
TRIM support/progress and errors, I/O rates, dataset space/property values and
their sources, ARC/L2ARC, and ATA/SAS/NVMe SMART data share one point-in-time
schema. Optional drive measurements and unavailable timestamps are absent.
Value-typed aggregate groups are interpreted together with their exact
`source_errors`; the exporter omits a failed group's samples instead of
presenting its wire zero values as measurements. Incomplete leaf TRIM state
aggregates to `mixed` or `unknown`. A valid NVMe `percentage_used` is
authoritative for wear. ATA wear uses explicitly allowlisted normalized 0-100
values, converts remaining life to used life, and leaves raw, invalid, or
conflicting candidates unknown.

## Monitoring data flow

When persisting, the fast collector skips SMART and safely carries forward
earlier hardware data only when stable ID, WWN, serial, and capacity still
match. The hardware collector selects an explicit device type: ATA/SATA uses
`--nocheck standby,2,2` to stop on unsupported power checks; NVMe uses
`--device nvme`. SAS/SCSI, USB, and unknown transports are skipped unless the
operator explicitly authorizes a waking refresh. SCSI has no equivalent
fail-closed unsupported-power-check option. A sleeping
disk keeps the prior values and their original timestamp while gaining a
sleeping marker. Never-collected or stale drive data becomes an unknown
finding. SMART timestamps more than one minute ahead also become unknown;
persisted JSON or database-fallback snapshots with missing/far-future
generation time or far-future SMART time are rejected. An invalid JSON cache
cannot satisfy exporter readiness; neither cache form can feed the next
carry-forward merge. Both cache forms must also declare the supported
`SnapshotV1` API version.

Persisted collectors take the collection lock before discovery and retain it
through merging and persistence. Apply-result cache writes use the same lock
and refuse to overwrite a snapshot with a newer generation time. Failed or
canceled collections preserve the previous cache. The
JSON snapshot uses write/fsync/rename replacement. Snapshot state uses SQLite
when its CLI is available; the fallback is an atomically replaced JSON file.
This shared state is cache, not bay or audit authority, and legacy database bay
rows are ignored. Manual mappings use a strict, bounded, atomically replaced
root-owned file below `/etc/zpoolpilot`; retiring an absent drive is an exact-key,
compare-and-delete operation rather than an alias lookup. Apply appends mandatory before/after
records to a separate root-owned JSONL file beneath `/var/log/zpoolpilot`. The
exporter loads the cache per request and never invokes hardware tools.
It recalculates SMART-age findings and drive-health labels at scrape time;
cached status and dashboard views use the same freshness evaluation.
Missing bay state is allowed; invalid or untrusted bay state is reported as a
partial `inventory.bays` trust error and no cached manual mapping is restored.
Exporter readiness requires a fresh, non-partial snapshot. Exact failures of
ARC, pool-list, pool-status, or pool-I/O collection suppress the corresponding
numeric Prometheus samples while leaving source-error metrics visible; a
successful text fallback is not suppressed merely because its JSON probe
failed.

Packaged execution is split three ways:

- the fast oneshot and exporter run as the `zpoolpilot` service account;
- the no-wake hardware oneshot runs briefly as root under the observer-only
  command policy; and
- mutation exists only in a foreground `sudo zpoolpilot apply` process attached
  to a real terminal.

## Plan/apply boundary

The planner stores typed intent, immutable device/pool references, safety
fingerprints, warnings/blockers, an expected result, and an exact argv preview.
It never stores shell text. A `DriveRef` pins stable identity, persistent by-id
path, capacity, logical/physical sector geometry, and major:minor number. A
plan file is untrusted input; its ID must be exactly 32 lowercase hexadecimal
characters.

Disk and topology operations are red. The five routine maintenance operations
(scrub start/stop, manual TRIM start/resume and suspend, and clear errors) are
yellow, but use the same expiring plan format and privileged apply boundary;
there is no direct maintenance mutation path.

Red lifecycle work blocks active or unknown scan/TRIM activity; pool creation
checks every imported pool for a conflicting operation. Yellow start/resume
plans likewise require authoritative state for the activity they coordinate.
In particular, scrub start requires both inactive scan state and confirmed
inactive manual TRIM; active, suspended, unknown, or uncertain mixed TRIM
blocks it.

Operations that consume new pool-member disks collect SMART with no-wake
`all`. Positive failed/critical target health is a plan blocker and is checked
again before dispatch; missing, sleeping, or unsupported SMART is warned but
not converted into a healthy result. `prepare-disk` remains available to
retire failed media, and routine maintenance uses fast discovery.

At apply time the privileged process validates schema and expiry, obtains an
exclusive lock, collects a fresh snapshot, reconstructs commands from a narrow
operation allowlist, and byte-compares them with the preview. Before each
command it confirms risk and rediscovers the host again. Target identity,
capacity, geometry, current use, host ID, and pool topology must still match.
Replace and attach require a nonzero selected-vdev size, while RAIDZ expansion
requires nonzero size authority for every child; these capacity inputs are
rechecked from fresh discovery before dispatch. Adding spares requires complete
data-leaf size authority and checks every target against the largest data leaf,
again during fresh apply revalidation.
The first mutation cannot run unless the before-audit succeeds; an after-audit
is attempted even after cancellation or command failure.

Immediately before every red command, a live disk-path guard verifies its
exact argv shape and re-resolves every persistent by-id link. The link must
remain a block-device symlink to the pinned major:minor and sysfs whole disk;
link/resolution/device changes during inspection fail the command.

Root processes require a singly linked, root-owned regular configuration file
and authenticate its complete directory ancestor chain, rejecting symlinks,
non-root ownership, and group/other write permission. They then confine
configured state/cache, plan/lock, and audit paths to their fixed application
trees. Encryption key files are rechecked on every safety refresh; privileged
apply requires the key and every directory component to meet the same
ownership, symlink, and write-permission rules.

Subprocesses use a root-controlled executable search path, direct argv, a
minimal locale environment, and bounded noninteractive capture (64 MiB stdout,
1 MiB stderr). Exceeding either limit fails the command. Before terminal
rendering, snapshot/cache/plan/error strings lose C0/C1 controls, escape bytes,
and Unicode directional controls; ANSI styling is generated internally.

Boot-pool replacement has its own typed operation. It validates Proxmox host
evidence, verified boot mode, persistent whole-disk paths, mirrored partition-3
membership, the healthy source sibling, and the clean replacement. Generic
pool operations remain blocked on boot-critical pools. Whole-disk identity is
revalidated before each step; after the kernel rereads the cloned table, a
last-mile guard proves the partition-2 and partition-3 links are block devices
owned by the exact planned whole disk before ESP or ZFS commands run.
