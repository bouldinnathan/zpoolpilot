# Threat and failure model

`zpoolpilot` treats command-line input, plan files, device names, ZFS object
names, udev properties, SMART JSON, and cached state as untrusted.

The implementation must preserve these invariants:

1. No shell evaluation or arbitrary executable path can originate in a plan.
2. A plan is advisory until an elevated apply rechecks host identity, stable
   drive identity, exact byte capacity and sector geometry, major:minor number,
   holders, mounts, swap, current pool membership, topology, and conflicting
   ZFS work.
3. A path match alone cannot authorize a disk change. Linux device names may
   be reused after hot-plug.
4. Observer mode has an allowlist independent of user-facing flags; a forged
   plan cannot make it execute mutation commands.
5. Encryption keys and passphrases never appear in argv, environment, command
   traces, state, metrics, or audit output.
6. Failure to parse or collect produces `unknown`/partial state, never a
   healthy zero value.
7. A root-pool problem cannot be repaired implicitly. Proxmox boot operations
   need their own preview and confirmations.
8. Untrusted display text cannot inject terminal control sequences or visually
   reorder trusted prompts.
9. Shared monitoring/inventory state cannot become the authoritative mutation
   audit or choose an arbitrary privileged write target.

Default SMART reads select ATA/SAT or NVMe explicitly to avoid potentially
waking device-type autodetection. ATA reads use `--nocheck standby,2,2` and
never retry without the power guard. SAS/SCSI and bridge/unknown transports
remain unknown unless an operator authorizes `--wake`; SCSI's unsupported
power-check fallback cannot provide the ATA stop behavior. Actual controller
and firmware behavior remains an unvalidated hardware boundary in this alpha.

Hard blockers cannot be overridden: an active system disk outside the boot
workflow, mounted/swap/LVM/MD use, ambiguous identity, unsafe or missing by-id
path, unknown capacity/geometry/device number, too-small replacement, stale
topology, missing selected-vdev or RAIDZ-child size authority, insufficient
surviving redundancy, a spare smaller than the largest data leaf, unknown
data-leaf size authority, or an active conflicting operation. Unknown
scan/TRIM authority also blocks destructive lifecycle work.
An unmounted raw ownership signature (including ZFS, LVM, MD, LUKS, Ceph,
BitLocker, or swap) is not assumed stale enough to wipe.

## Trust boundaries

The dashboard and exporter may display a cached snapshot, but cached data never
authorizes a storage change. Planning collects fresh non-waking data, and apply
collects again before its initial authorization and immediately before every
command. Discovery failure in machine identity, pool/dataset topology, block
topology, by-id correlation, drive identity, or udev state blocks planning and
apply. Optional SMART, ARC, I/O-rate, and Proxmox probes can instead make a
monitoring snapshot partial. Positive failed/critical target-drive health is
also a hard blocker for operations that consume a new pool member and is
rechecked before dispatch; missing/sleeping/unsupported target SMART is an
explicit warning, not healthy evidence. Boot source health remains mandatory.
`prepare-disk` stays available for retiring failed media.

OpenZFS JSON records have required structural minima, including pool/vdev
identity and state, leaf paths, scan authority, bootfs, dataset GUID/type, and
filesystem mountpoint. Status/list/get sources must agree bidirectionally on
each pool name and GUID, and dataset coverage must retain bootfs and `/`
evidence. Empty JSON is accepted only when independent source output confirms
there are no objects.

`/proc/self/mountinfo` is a separate root-filesystem authority. An actual ZFS
mount at `/` marks its source pool boot-critical regardless of `bootfs` or
dataset mountpoint properties. Missing, malformed, ambiguous, or unmatched
root-mount evidence makes discovery partial and blocks planning.

Plan JSON is limited to 1 MiB, loaded without following a final symlink, must
be a regular file not writable by group/other, has unknown fields rejected,
uses an exact 32-lowercase-hex-character plan ID, and expires after no more
than 15 minutes. Serialized drive references include capacity, logical and
physical sector geometry, by-id path, and major:minor identity. The stored argv
is not trusted: apply rebuilds it from typed intent and rejects any difference.
An exclusive apply lock prevents two plans executing concurrently.

The subprocess boundary accepts only a bare executable plus an argument array.
Observer and mutation allowlists are independent, with additional exact-shape
rules for disk paths, Proxmox partitions, SES LED changes, and routine
maintenance. Executables resolve only through a fixed system path; the child
environment and noninteractive output are bounded, and capture overflow fails
the command. `--read-only` rejects mutation capability even if an outer CLI
check were bypassed.

An inferred bay label cannot authorize SES locate. LED control requires a
manual verified mapping with timestamp, a nonempty label and nonnegative slot,
and a live direct character device at canonical `/dev/sgN` or
`/dev/bsg/H:C:T:L`; device symlinks are rejected. Physical verification pins a
udev WWN or hardware serial from a SCSI enclosure-services type-13 device.
Locate compares that identity before operator confirmation and again
immediately before dispatch. Turning an identify LED on requires a present,
strongly identified drive and physical verification within the last 15 minutes;
after confirmation it also reloads the authoritative mapping and freshly
revalidates the selected drive's stable identity and major:minor. Turning the
LED off instead requires the exact stored key but permits an absent drive or
older verification so a stale indicator is recoverable. It still reloads and
compares the trusted record and repeats structural and SES-identity checks
immediately before dispatch. Timestamps more than one minute ahead are rejected
for both states.

The LED remains guidance only: the operator must match the physical bay label
and drive serial and must never pull a disk solely because the LED is lit.
Map and verify likewise recollect after the prompt and require the exact drive
fingerprint and major:minor to remain stable; verify re-probes enclosure
identity immediately before storing it.

Manual bay authority is the versioned, bounded `/etc/zpoolpilot/bays.json`, not
the shared database or a cached snapshot. Only root may write it, and map,
verify, unmap, and locate require a real TTY. Read-only `mappings` exposes the
exact stable keys for absent drives. Unmap never resolves a devnode or alias:
after yellow confirmation it reloads the selected record, then compare-and-
deletes that exact key while holding the file writer lock. Reads reject
symlinks, unexpected fields, an unsafe owner/type/mode/link count, or an
untrusted directory path; writes are locked and atomically replaced. Legacy
database bay rows are ignored and must be remapped and verified. A missing
file means no manual mappings; a malformed or untrusted file produces partial
`inventory.bays`/`trust` state without accepting cached manual values.
An active identify LED must be cleared before unmap because deletion removes
the only accepted slot authority.

When effective UID is root, the config must be a root-owned, singly linked
regular file that is not group/other-writable. Its entire directory ancestor
chain must be root-owned, nonsymlinked, and not writable by group/other.
Configured writes remain beneath the fixed
`/var/lib/zpoolpilot`, `/run/zpoolpilot`, `/var/log/zpoolpilot`, or
`/etc/zpoolpilot` subtree for their role. A privileged keyfile plan applies the
same directory-chain rules to a root-owned singly linked key file and rechecks
it before commands.

Terminal renderers replace escape/C0/C1 and Unicode bidirectional formatting
controls in discovery, cache, plan, command, and error strings. Color escape
sequences are generated only by the renderer, so disabling color produces no
ANSI output from attacker-controlled fields.

## Monitoring failure behavior

The exporter has no device access and reads only the latest cache. Fast and
root hardware collectors serialize cache/state writes using a no-symlink,
singly-linked regular lock file. A fast pass carries SMART data forward only
for an exact stable-identity match; a sleeping drive preserves the prior
timestamp. Never-collected and stale data becomes unknown. Thus a race, disk
replacement, standby result, unsupported firmware field, permission failure,
or parse error cannot fabricate a healthy zero.

Wear normalization prefers explicit NVMe `percentage_used`. ATA vendor
attributes are accepted only by name allowlist from their normalized top-level
0-100 value; raw counters, invalid ranges, and disagreeing candidates remain
unknown.

A SMART timestamp more than one minute ahead produces an unknown freshness
finding. Cache loading rejects a missing or far-future snapshot generation
time and any far-future SMART time. An invalid JSON cache makes readiness
unavailable; the database fallback receives the same timestamp validation, and
both require the supported `SnapshotV1` API version. Suspect telemetry from
either cache is not merged forward.

Collector health exits 1-3 are distinct from operational failures: collection
incompletion is 69 and persistence failure is 74, so systemd does not mistake
them for warning/critical/unknown health snapshots.

`/readyz` refuses partial snapshots even when their timestamps are fresh.
Prometheus omits ARC, pool capacity, pool status, and pool I/O samples whose
authoritative source failed, while retaining explicit source-error metrics.
Failed JSON probes do not suppress measurements recovered by a successful
compatibility fallback.

The shared state database and snapshot are replaceable cache state, not bay,
authorization, or audit authority. Mutation attempts receive mandatory
before/after records in a root-owned, root-writable JSONL file under
`/var/log/zpoolpilot`; failure to write the before record prevents mutation.

## Boot-specific boundary

Generic plans cannot mutate a boot-critical pool. `replace-boot-disk` requires
Proxmox host evidence, one consistent `proxmox-boot-tool` mode, a mirrored
partition-3 layout, a healthy online source sibling, and a clean, sufficiently
large, distinct replacement whole disk. Its fixed sequence cannot dispatch
plain `grub-install` or perform repair/mode conversion. The topology-changing
`zpool replace` is last, after partition and ESP setup; completion still
requires an operator to verify resilvering and a controlled reboot.

Every red command has a last-mile disk-path guard: each referenced by-id link
must remain a symlink to the pinned block-device major:minor and sysfs whole
disk immediately before dispatch. Boot steps additionally prove that the new
partition-2 and partition-3 links belong to that exact whole disk after
`partprobe`.

Routine scrub/TRIM/error-counter operations are yellow but retain the same
plan/apply trust boundary. Their allowlist accepts only the five exact forms;
recovery flags such as `zpool clear -F` are outside this workflow.
Scrub start is allowed only when scan state and manual-TRIM inactivity are
authoritative; active, suspended, unknown, or uncertain mixed TRIM blocks it.
