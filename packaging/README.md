# Packaging and service model

The v1 package targets x86-64 Linux systems using systemd and OpenZFS 2.3 or
newer, with Proxmox VE 9 as the primary deployment. The default package prefix
is `/usr`; the administrator configuration remains in `/etc/zpoolpilot` and is
not overwritten by a repeated `make install`.

Because the shipped units intentionally use fixed, reviewable executable and
configuration paths, the v1 install target rejects `BINDIR` values other than
`/usr/bin` and `SYSCONFDIR` values other than `/etc`. Use `DESTDIR` to stage a
package without changing those runtime paths.

## Runtime dependencies

Required commands for full collection are `zpool`, `zfs`, `lsblk`, `udevadm`,
and `smartctl`. SQLite's CLI is recommended for shared snapshot cache state;
the application has an atomic JSON fallback. Authoritative manual bay mappings
use the separate root-owned `/etc/zpoolpilot/bays.json`; legacy database bay rows
are ignored. The authoritative mutation audit is a separate root-owned JSONL
file. `sgdisk` (from gdisk) and `wipefs` (normally from util-linux) are needed
for `prepare-disk`. The guarded Proxmox boot-disk workflow also needs `sgdisk`,
`partprobe`, `pveversion`, and `proxmox-boot-tool`. `sg_ses` is optional for
verified enclosure identify LEDs. Proxmox storage registration is not
implemented.

The package installs:

- `/usr/bin/zpoolpilot`;
- `/etc/zpoolpilot/config.toml`;
- an empty versioned `/etc/zpoolpilot/bays.json` mapping store;
- schemas and documentation below `/usr/share`;
- one unprivileged exporter, two short-lived collectors, and their timers;
- sysusers and tmpfiles declarations for the `zpoolpilot` account and state.

Stage the package payload without enabling or starting anything:

```sh
make VERSION=1.0.0 COMMIT=abcdef BUILD_DATE=2026-09-03T00:00:00Z
make DESTDIR="$PWD/pkgroot" install
```

Distribution packages should mark `/etc/zpoolpilot/config.toml` as a conffile.
They must likewise preserve `/etc/zpoolpilot/bays.json` as administrator state.
They should run the platform's normal sysusers/tmpfiles integration and reload
systemd after installation. Installation intentionally does not enable units.
A root invocation authenticates this configuration as a singly linked,
root-owned regular file with no group/other write permission. Its entire
directory ancestor chain must also be root-owned, nonsymlinked, and not
group/other-writable. Privileged configured paths must remain in their dedicated
`/var/lib/zpoolpilot`, `/run/zpoolpilot`, `/var/log/zpoolpilot`, and
`/etc/zpoolpilot` subtrees; packaging must preserve those paths and ownerships.

A typical administrator opt-in is:

```sh
systemctl enable --now zpoolpilot-collect-fast.timer
systemctl enable --now zpoolpilot-collect-hardware.timer
systemctl enable --now zpoolpilot-exporter.service
```

The packaged exporter is loopback-only by default. To permit a central
Prometheus server, set `monitor.listen` to a dedicated management address and
use a systemd drop-in containing a narrowly scoped additional
`IPAddressAllow=ADDRESS_OR_CIDR`; the existing `IPAddressDeny=any` remains the
default-deny rule. Prefer a local authenticated/TLS reverse proxy if that
network is not trusted. Metrics contain hardware serials and bay placement,
so a public listener is intentionally not shipped.

## Privilege boundaries

- `zpoolpilot-collect-fast.service` runs as the unprivileged `zpoolpilot` user.
- `zpoolpilot-collect-hardware.service` is the only root monitoring unit. It is
  short-lived, invokes the CLI with `--read-only`, has a restricted capability
  bounding set, and exists because SMART/controller access is not portable
  across device drivers and HBAs.
- `zpoolpilot-exporter.service` is unprivileged, has no device namespace or
  capabilities, and only reads the cached snapshot.
- Storage changes are never performed by a resident service. An operator must
  invoke `sudo zpoolpilot apply PLAN` from a real terminal.
- Bay map, physical verification, exact-key unmap, and SES locate likewise
  require root, a real terminal, and interactive yellow confirmation.
  Read-only mapping output includes absent-drive keys; monitoring services may
  read the authoritative bay file but cannot write it.

Collector exit codes 1, 2, and 3 communicate warning, critical, and unknown
health after a snapshot was successfully produced. The service units therefore
declare those values in `SuccessExitStatus`; argument/configuration failures
and collection/persistence failures (69/74) still fail the unit. Both collectors
take the application's collection lock in `/run/zpoolpilot/collect` and use
`--wait`, so a rare timer overlap is serialized rather than reported as a
failed collection. Snapshot and fallback-state writes cannot overlap.
Tmpfiles pre-creates the shared state database and collection lock as mode
`0660`, owned by `zpoolpilot:zpoolpilot`. The state directory is setgid and
group-writable so cache files produced by the root hardware collector remain
readable by the unprivileged exporter. This database is not bay authority.
Installation seeds an empty versioned `bays.json` only when none exists;
reinstallation preserves it. Root mapping operations lock and atomically
replace the root-owned, non-group/other-writable file. Unmap compare-and-deletes
the expected record under that lock so a retired drive's bay can be reused
without manually editing the file. The parent
`/run/zpoolpilot` directory and apply-plan directory remain root-owned and are
not group-writable. The
authoritative `/var/log/zpoolpilot/audit.jsonl` is mode `0640`, root-owned, and
not writable by the monitoring account; apply must append its before record
before the first mutation.

Run `make check-packaging` and `make install-check` before building a distro
package. See `docs/testing.md` for the opt-in disposable-VM integration suite.

The normal release gate is fully offline and non-destructive:

```sh
make check
make test-race
```

The live integration target is excluded from that gate. Its presence is not a
claim that a live destructive test was run. It requires root, explicit
acknowledgement variables, VM detection, OpenZFS, and dedicated temporary loop
devices; it must never run on a production storage host.
The gate does compile the integration tests without executing them, through
`make test-integration-build`.
