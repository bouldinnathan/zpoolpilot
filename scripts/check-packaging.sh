#!/usr/bin/env bash
# Copyright (C) 2026 ZpoolPilot contributors
# SPDX-License-Identifier: GPL-3.0-or-later

set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
cd -- "${repo_root}"

fail() {
	printf 'packaging check: %s\n' "$*" >&2
	exit 1
}

require_line() {
	local file="$1"
	local line="$2"
	grep -Fqx -- "${line}" "${file}" || fail "${file} must contain: ${line}"
}

services=(packaging/systemd/*.service)
timers=(packaging/systemd/*.timer)

for unit in "${services[@]}"; do
	require_line "${unit}" 'NoNewPrivileges=yes'
	require_line "${unit}" 'ProtectSystem=strict'
	require_line "${unit}" 'ProtectHome=yes'
	require_line "${unit}" 'RestrictSUIDSGID=yes'
	require_line "${unit}" 'RestrictNamespaces=yes'
	require_line "${unit}" 'LockPersonality=yes'
	require_line "${unit}" 'SystemCallArchitectures=native'
	require_line "${unit}" 'MemoryDenyWriteExecute=yes'
	if grep -Fq '/usr/local/' "${unit}"; then
		fail "${unit} uses /usr/local but the package installs under /usr"
	fi
done

require_line packaging/systemd/zpoolpilot-collect-fast.service 'User=zpoolpilot'
require_line packaging/systemd/zpoolpilot-collect-fast.service 'CapabilityBoundingSet='
require_line packaging/systemd/zpoolpilot-collect-fast.service 'SuccessExitStatus=1 2 3'
require_line packaging/systemd/zpoolpilot-collect-fast.service 'ReadWritePaths=/var/lib/zpoolpilot /run/zpoolpilot/collect'
require_line packaging/systemd/zpoolpilot-collect-fast.service 'ExecStart=/usr/bin/zpoolpilot --config /etc/zpoolpilot/config.toml --read-only monitor collect --profile fast --wait'

require_line packaging/systemd/zpoolpilot-collect-hardware.service 'User=root'
require_line packaging/systemd/zpoolpilot-collect-hardware.service 'CapabilityBoundingSet=CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_SYS_ADMIN CAP_SYS_RAWIO'
require_line packaging/systemd/zpoolpilot-collect-hardware.service 'SuccessExitStatus=1 2 3'
require_line packaging/systemd/zpoolpilot-collect-hardware.service 'ReadWritePaths=/var/lib/zpoolpilot /run/zpoolpilot/collect'
require_line packaging/systemd/zpoolpilot-collect-hardware.service 'ExecStart=/usr/bin/zpoolpilot --config /etc/zpoolpilot/config.toml --read-only monitor collect --profile hardware --wait'

require_line packaging/systemd/zpoolpilot-exporter.service 'User=zpoolpilot'
require_line packaging/systemd/zpoolpilot-exporter.service 'SuccessExitStatus=130'
require_line packaging/systemd/zpoolpilot-exporter.service 'PrivateDevices=yes'
require_line packaging/systemd/zpoolpilot-exporter.service 'CapabilityBoundingSet='
require_line packaging/systemd/zpoolpilot-exporter.service 'IPAddressAllow=localhost'
require_line packaging/systemd/zpoolpilot-exporter.service 'ExecStart=/usr/bin/zpoolpilot --config /etc/zpoolpilot/config.toml --read-only monitor serve'

require_line packaging/tmpfiles.d/zpoolpilot.conf 'd /var/lib/zpoolpilot 2770 zpoolpilot zpoolpilot -'
require_line packaging/tmpfiles.d/zpoolpilot.conf 'f /var/lib/zpoolpilot/state.db 0660 zpoolpilot zpoolpilot -'
require_line packaging/tmpfiles.d/zpoolpilot.conf 'f /var/log/zpoolpilot/audit.jsonl 0640 root zpoolpilot -'
require_line packaging/tmpfiles.d/zpoolpilot.conf 'd /run/zpoolpilot 0750 root zpoolpilot -'
require_line packaging/tmpfiles.d/zpoolpilot.conf 'd /run/zpoolpilot/collect 2750 zpoolpilot zpoolpilot -'
require_line packaging/tmpfiles.d/zpoolpilot.conf 'f /run/zpoolpilot/collect/collect.lock 0660 zpoolpilot zpoolpilot -'
require_line packaging/tmpfiles.d/zpoolpilot.conf 'd /run/zpoolpilot/plans 0750 root zpoolpilot 1h'

for timer in "${timers[@]}"; do
	require_line "${timer}" 'Persistent=true'
done

for script in scripts/*.sh; do
	[[ -x "${script}" ]] || fail "script must be executable: ${script}"
	bash -n "${script}" || fail "bash syntax failed for ${script}"
done

scratch="$(mktemp -d /tmp/zpoolpilot-packaging-check.XXXXXXXX)"
case "${scratch}" in
	/tmp/zpoolpilot-packaging-check.*) ;;
	*) fail "unexpected temporary directory ${scratch}" ;;
esac
cleanup() {
	find "${scratch}" -depth -delete 2>/dev/null || true
}
trap cleanup EXIT

install -d \
	"${scratch}/usr/bin" \
	"${scratch}/usr/lib/systemd/system" \
	"${scratch}/usr/lib/sysusers.d" \
	"${scratch}/usr/lib/tmpfiles.d"
install -m0755 /dev/null "${scratch}/usr/bin/zpoolpilot"
install -m0644 packaging/systemd/* "${scratch}/usr/lib/systemd/system/"
install -m0644 packaging/sysusers.d/zpoolpilot.conf "${scratch}/usr/lib/sysusers.d/zpoolpilot.conf"
install -m0644 packaging/tmpfiles.d/zpoolpilot.conf "${scratch}/usr/lib/tmpfiles.d/zpoolpilot.conf"

# Verification runs against an isolated root. Supply only the external targets
# referenced by systemd's default service/timer dependencies and our units;
# their host implementations are outside this package's validation scope.
for target in sysinit basic shutdown timers network zfs-import; do
	cat >"${scratch}/usr/lib/systemd/system/${target}.target" <<EOF
[Unit]
Description=Packaging verification fixture for ${target}.target
DefaultDependencies=no
EOF
done

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers --root="${scratch}" >/dev/null 2>&1
fi
if command -v systemd-tmpfiles >/dev/null 2>&1; then
	systemd-tmpfiles --root="${scratch}" --create --dry-run >/dev/null 2>&1
fi

filter_sandbox_noise() {
	sed \
		-e '/Failed to turn off SO_PASSRIGHTS on user lookup socket/d' \
		-e '/Failed to enable SO_PASSCRED on handoff timestamp socket/d' \
		-e '/Failed to connect to audit log, ignoring/d' \
		-e '/^[[:space:]]*$/d'
}

if command -v systemd-analyze >/dev/null 2>&1; then
	set +e
	verify_output="$(systemd-analyze --root="${scratch}" --man=no verify \
		zpoolpilot-collect-fast.service zpoolpilot-collect-fast.timer \
		zpoolpilot-collect-hardware.service zpoolpilot-collect-hardware.timer \
		zpoolpilot-exporter.service 2>&1)"
	verify_status=$?
	set -e
	filtered="$(printf '%s\n' "${verify_output}" | filter_sandbox_noise)"
	if [[ ${verify_status} -ne 0 && -n "${filtered}" ]]; then
		printf '%s\n' "${filtered}" >&2
		fail 'systemd-analyze verify failed'
	fi
	if [[ ${verify_status} -ne 0 ]]; then
		printf '%s\n' 'packaging check: systemd-analyze unavailable in this sandbox; static checks passed' >&2
	fi

	if systemd-analyze security --help 2>&1 | grep -- '--offline' >/dev/null; then
		for service in zpoolpilot-collect-fast.service zpoolpilot-collect-hardware.service zpoolpilot-exporter.service; do
			set +e
			# The CLI threshold uses tenths: 70 means the documented 7.0 score.
			security_output="$(systemd-analyze --root="${scratch}" security --offline=yes --threshold=70 "${service}" 2>&1)"
			security_status=$?
			set -e
			filtered="$(printf '%s\n' "${security_output}" | filter_sandbox_noise)"
			if [[ ${security_status} -ne 0 && -n "${filtered}" ]]; then
				printf '%s\n' "${filtered}" >&2
				fail "systemd security analysis failed for ${service}"
			fi
		done
	fi
fi

printf '%s\n' 'packaging check: service, sysusers, and tmpfiles policy passed'
