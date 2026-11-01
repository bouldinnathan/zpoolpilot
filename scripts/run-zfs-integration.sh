#!/usr/bin/env bash
# Copyright (C) 2026 ZpoolPilot contributors
# SPDX-License-Identifier: GPL-3.0-or-later

set -Eeuo pipefail

readonly acknowledgement='DESTROY_TEMPORARY_ZFS_TEST_POOLS'
readonly vm_attestation='ephemeral'

fail() {
	printf 'ZFS integration guard: %s\n' "$*" >&2
	exit 1
}

[[ "${ZPOOLPILOT_DESTRUCTIVE_TESTS:-}" == "${acknowledgement}" ]] ||
	fail "set ZPOOLPILOT_DESTRUCTIVE_TESTS=${acknowledgement}"
[[ "${ZPOOLPILOT_INTEGRATION_VM:-}" == "${vm_attestation}" ]] ||
	fail "set ZPOOLPILOT_INTEGRATION_VM=${vm_attestation} only inside a disposable VM"
[[ "$(id -u)" == 0 ]] || fail 'effective UID must be root'

for command in go losetup lsblk realpath smartctl systemd-detect-virt timeout truncate udevadm zfs zpool; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command is missing: ${command}"
done

container="$(systemd-detect-virt --container 2>/dev/null || true)"
[[ -z "${container}" || "${container}" == none ]] || fail "containers are not accepted (${container})"
virtualization="$(systemd-detect-virt --vm 2>/dev/null || true)"
[[ -n "${virtualization}" && "${virtualization}" != none ]] || fail 'systemd-detect-virt did not identify a VM'

zfs_version_output="$(zpool --version 2>&1)"
if [[ "${zfs_version_output}" =~ zfs-([0-9]+)\.([0-9]+) ]]; then
	zfs_major="${BASH_REMATCH[1]}"
	zfs_minor="${BASH_REMATCH[2]}"
else
	fail 'could not parse OpenZFS version'
fi
((zfs_major > 2 || (zfs_major == 2 && zfs_minor >= 3))) || fail 'OpenZFS 2.3 or newer is required'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
binary="$(realpath -e -- "${ZPOOLPILOT_INTEGRATION_BINARY:-${repo_root}/bin/zpoolpilot}")"
[[ -x "${binary}" ]] || fail "integration binary is not executable: ${binary}"
case "${binary}" in
	"${repo_root}"/*) ;;
	*) fail 'integration binary must be inside the checked-out repository' ;;
esac

run_dir="$(mktemp -d /var/tmp/zpoolpilot-integration.XXXXXXXX)"
case "${run_dir}" in
	/var/tmp/zpoolpilot-integration.*) ;;
	*) fail "unexpected integration directory: ${run_dir}" ;;
esac
chmod 0700 "${run_dir}"

# The binary runs as root in this guarded VM harness, so its temporary write
# targets must obey the same fixed-subtree confinement as a real installation.
[[ -d /var/lib/zpoolpilot ]] || install -d -m 0750 /var/lib/zpoolpilot
[[ -d /run/zpoolpilot ]] || install -d -m 0750 /run/zpoolpilot
[[ -d /var/log/zpoolpilot ]] || install -d -m 0750 /var/log/zpoolpilot
[[ -d /etc/zpoolpilot ]] || install -d -m 0755 /etc/zpoolpilot
state_dir="$(mktemp -d /var/lib/zpoolpilot/integration.XXXXXXXX)"
runtime_dir="$(mktemp -d /run/zpoolpilot/integration.XXXXXXXX)"
audit_dir="$(mktemp -d /var/log/zpoolpilot/integration.XXXXXXXX)"
config_dir="$(mktemp -d /etc/zpoolpilot/integration.XXXXXXXX)"
chmod 0700 "${state_dir}" "${runtime_dir}" "${audit_dir}" "${config_dir}"

pool_name="zpoolpilot_ci_$(date -u +%s)_$$_${RANDOM}"
[[ "${pool_name}" =~ ^zpoolpilot_ci_[0-9]+_[0-9]+_[0-9]+$ ]] || fail 'generated pool name failed validation'

images=()
loops=()
pool_creation_attempted=0
pool_guid=''
config_path="${config_dir}/zpoolpilot.toml"
bay_path="${config_dir}/bays.json"
state_path="${state_dir}/state.db"
snapshot_path="${state_dir}/latest.json"
plans_path="${runtime_dir}/plans"
lock_path="${runtime_dir}/apply.lock"
collection_lock_path="${runtime_dir}/collect.lock"
audit_path="${audit_dir}/audit.jsonl"

cleanup() {
	local original_status=$?
	local cleanup_failed=0
	local remove_backing=1
	local detach_failed=0
	local current_guid=''
	local loop_device=''
	local backing=''
	trap - EXIT HUP INT TERM
	set +e

	if [[ ${pool_creation_attempted} -eq 1 ]]; then
		current_guid="$(timeout 30s zpool get -H -o value guid "${pool_name}" 2>/dev/null)"
		if [[ -n "${pool_guid}" && "${current_guid}" == "${pool_guid}" && "${pool_name}" =~ ^zpoolpilot_ci_[0-9]+_[0-9]+_[0-9]+$ ]]; then
			if ! timeout 60s zpool destroy -f "${pool_name}"; then
				cleanup_failed=1
				remove_backing=0
			fi
		else
			# A failed lookup is not proof that the pool is absent. Creation can
			# succeed in the kernel before timeout or a signal interrupts its
			# caller, leaving no recorded GUID. Preserve the loop backing files
			# whenever ownership or successful destruction cannot be proven.
			printf 'ZFS integration cleanup: preserving backing for %s because its GUID changed, was not recorded, or could not be read\n' "${pool_name}" >&2
			cleanup_failed=1
			remove_backing=0
		fi
	fi

	if [[ ${remove_backing} -eq 1 ]]; then
		for loop_device in "${loops[@]}"; do
			if ! losetup "${loop_device}" >/dev/null 2>&1; then
				continue
			fi
			backing="$(losetup --noheadings --raw --output BACK-FILE "${loop_device}" 2>/dev/null)"
			backing="$(realpath -e -- "${backing}" 2>/dev/null)"
			case "${backing}" in
				"${run_dir}"/*)
					if ! losetup -d "${loop_device}"; then
						cleanup_failed=1
						detach_failed=1
					fi
					;;
				*)
					printf 'ZFS integration cleanup: refusing to detach %s with unexpected backing %s\n' "${loop_device}" "${backing}" >&2
					cleanup_failed=1
					detach_failed=1
					;;
			esac
		done

		if [[ ${detach_failed} -eq 0 ]]; then
			rm -f -- "${images[@]}" "${config_path}" "${bay_path}" "${bay_path}.lock" "${state_path}" "${state_path}.json" \
				"${state_path}-journal" "${state_path}-shm" "${state_path}-wal" \
				"${snapshot_path}" "${lock_path}" "${collection_lock_path}" "${audit_path}"
			rmdir -- "${plans_path}" 2>/dev/null || true
			rmdir -- "${state_dir}" 2>/dev/null || cleanup_failed=1
			rmdir -- "${runtime_dir}" 2>/dev/null || cleanup_failed=1
			rmdir -- "${audit_dir}" 2>/dev/null || cleanup_failed=1
			rmdir -- "${config_dir}" 2>/dev/null || cleanup_failed=1
			rmdir -- "${run_dir}" 2>/dev/null || cleanup_failed=1
		fi
	fi

	if [[ ${cleanup_failed} -ne 0 ]]; then
		printf 'ZFS integration cleanup was incomplete; destroy the disposable VM before reuse\n' >&2
		[[ ${original_status} -ne 0 ]] || original_status=1
	fi
	exit "${original_status}"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

{
	printf '[paths]\n'
	printf 'state = "%s"\n' "${state_path}"
	printf 'snapshot = "%s"\n' "${snapshot_path}"
	printf 'bays = "%s"\n' "${bay_path}"
	printf 'plans = "%s"\n' "${plans_path}"
	printf 'lock = "%s"\n' "${lock_path}"
	printf 'audit = "%s"\n' "${audit_path}"
	printf 'collection_lock = "%s"\n' "${collection_lock_path}"
	printf '\n[monitor]\n'
	# The integration suite never starts the exporter, but the generated file
	# must still pass the same configuration validation as production.
	printf 'listen = "127.0.0.1:19815"\n'
	printf 'fast_interval = "1m"\n'
	printf 'hardware_interval = "15m"\n'
	printf 'command_timeout = "30s"\n'
	printf 'wake_disks = false\n'
	printf 'include_command_trace = true\n'
	printf '\n[thresholds]\n'
	printf 'capacity_warning = 80\n'
	printf 'capacity_critical = 90\n'
	printf 'wear_warning = 80\n'
	printf 'wear_critical = 90\n'
	printf 'fragmentation_advisory = 70\n'
} >"${config_path}"
chmod 0600 "${config_path}"
printf '{"api_version":"github.com/bouldinnathan/zpoolpilot/bays/v1","mappings":{}}\n' >"${bay_path}"
chmod 0644 "${bay_path}"

# Validate every generated path and setting before attaching a loop device or
# creating a pool. Doctor's JSON path loads configuration without mutating ZFS.
"${binary}" --config "${config_path}" --read-only doctor --json >/dev/null

if zpool list -H -o name "${pool_name}" >/dev/null 2>&1; then
	fail "generated pool already exists: ${pool_name}"
fi

attach_loop() {
	local label="$1"
	local image="${run_dir}/${label}.img"
	local loop_device=''
	local backing=''

	truncate -s 768M "${image}"
	image="$(realpath -e -- "${image}")"
	case "${image}" in
		"${run_dir}"/*) ;;
		*) fail "backing file escaped integration directory: ${image}" ;;
	esac
	images+=("${image}")
	loop_device="$(losetup --find --show --nooverlap "${image}")"
	[[ "${loop_device}" =~ ^/dev/loop[0-9]+$ && -b "${loop_device}" ]] || fail "unexpected loop device: ${loop_device}"
	loops+=("${loop_device}")
	backing="$(losetup --noheadings --raw --output BACK-FILE "${loop_device}")"
	backing="$(realpath -e -- "${backing}")"
	[[ "${backing}" == "${image}" ]] || fail "loop backing mismatch for ${loop_device}"
}

attach_loop disk_a
attach_loop disk_b
attach_loop disk_spare
attach_loop disk_replacement

for index in 0 1 2 3; do
	backing="$(losetup --noheadings --raw --output BACK-FILE "${loops[index]}")"
	backing="$(realpath -e -- "${backing}")"
	[[ "${backing}" == "${images[index]}" ]] || fail "vdev ${loops[index]} no longer has its recorded backing file"
done

printf 'ZFS integration: VM=%s pool=%s directory=%s\n' "${virtualization}" "${pool_name}" "${run_dir}"
# Set this before dispatch: a failed or interrupted create may still have
# created an imported pool and must never authorize deleting its backing.
pool_creation_attempted=1
timeout 60s zpool create -f -o ashift=12 -o cachefile=none \
	-O mountpoint=none -O canmount=off "${pool_name}" mirror "${loops[0]}" "${loops[1]}"
pool_guid="$(zpool get -H -o value guid "${pool_name}")"
[[ "${pool_guid}" =~ ^[0-9]+$ ]] || fail 'created pool did not report a numeric GUID'

timeout 30s zfs create -o mountpoint=none "${pool_name}/integration"
timeout 30s zpool add "${pool_name}" spare "${loops[2]}"
timeout 120s zpool replace -w "${pool_name}" "${loops[0]}" "${loops[3]}"
timeout 30s zpool sync "${pool_name}"
[[ "$(zpool list -H -o health "${pool_name}")" == ONLINE ]] || fail 'temporary pool is not ONLINE after replacement'

export ZPOOLPILOT_INTEGRATION_POOL="${pool_name}"
export ZPOOLPILOT_INTEGRATION_POOL_GUID="${pool_guid}"
export ZPOOLPILOT_INTEGRATION_CONFIG="${config_path}"
export ZPOOLPILOT_INTEGRATION_BINARY="${binary}"

cd -- "${repo_root}"
GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
	go test -buildvcs=false -mod=readonly -tags=zfsintegration -count=1 -v ./integration

printf 'ZFS integration: live collection and read-only boundary passed\n'
