#!/usr/bin/env bash
# Copyright (C) 2026 ZpoolPilot contributors
# SPDX-License-Identifier: GPL-3.0-or-later

set -Eeuo pipefail

# Re-run after copying the checkout into a freshly reset disposable guest.
# No hypervisor APIs, guest creation, physical disks, or repository publishing.
fail() { printf 'Proxmox lab setup: %s\n' "$*" >&2; exit 1; }

[[ "${ZPOOLPILOT_LAB_SETUP:-}" == I_CONFIRM_DISPOSABLE_PROXMOX_VM ]] ||
	fail 'set ZPOOLPILOT_LAB_SETUP=I_CONFIRM_DISPOSABLE_PROXMOX_VM only after independently confirming the guest is disposable'
[[ "$(id -u)" == 0 ]] || fail 'root is required inside the disposable guest'
command -v pveversion >/dev/null || fail 'Proxmox VE is required'
container="$(systemd-detect-virt --container 2>/dev/null || true)"
[[ -z "${container}" || "${container}" == none ]] || fail 'containers are not supported'
virtualization="$(systemd-detect-virt --vm 2>/dev/null || true)"
[[ -n "${virtualization}" && "${virtualization}" != none ]] || fail 'a virtual machine is required'
[[ -z "$(qm list | tail -n +2)" ]] || fail 'VM inventory is not empty'
[[ -z "$(pct list | tail -n +2)" ]] || fail 'container inventory is not empty'
[[ -z "$(zpool list -H -o name)" ]] || fail 'imported pools exist; use a freshly reset lab guest'
if findmnt -rn -t nfs,nfs4,cifs,9p,virtiofs | grep . >/dev/null; then
	fail 'shared filesystems are mounted'
fi

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
[[ -f "${repo_root}/go.mod" ]] || fail 'run from a copied ZpoolPilot source tree'

# Use only Debian's configured public distribution sources for prerequisites;
# leave Proxmox subscription and Ceph sources and host configuration untouched.
[[ -f /etc/apt/sources.list.d/debian.sources ]] || fail 'expected Debian sources are missing'
apt_options=(-o Dir::Etc::sourcelist=sources.list.d/debian.sources -o Dir::Etc::sourceparts=-)
export DEBIAN_FRONTEND=noninteractive
apt-get "${apt_options[@]}" update
apt-get "${apt_options[@]}" install -y --no-install-recommends \
	golang-go make gcc libc6-dev smartmontools sqlite3 gdisk parted udev util-linux

pveversion
go version
zpool --version
printf '%s\n' 'Proxmox lab setup: prerequisites ready; no storage operations performed'
