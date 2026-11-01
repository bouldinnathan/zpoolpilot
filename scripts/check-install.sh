#!/usr/bin/env bash
# Copyright (C) 2026 ZpoolPilot contributors
# SPDX-License-Identifier: GPL-3.0-or-later

set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
cd -- "${repo_root}"

fail() {
	printf 'install check: %s\n' "$*" >&2
	exit 1
}

stage="$(mktemp -d /tmp/zpoolpilot-install-check.XXXXXXXX)"
case "${stage}" in
	/tmp/zpoolpilot-install-check.*) ;;
	*) fail "unexpected temporary directory ${stage}" ;;
esac
cleanup() {
	find "${stage}" -depth -delete 2>/dev/null || true
}
trap cleanup EXIT

make_command="${MAKE:-make}"
"${make_command}" --no-print-directory DESTDIR="${stage}" install >/dev/null

required=(
	usr/bin/zpoolpilot
	etc/zpoolpilot/config.toml
	etc/zpoolpilot/bays.json
	usr/lib/systemd/system/zpoolpilot-collect-fast.service
	usr/lib/systemd/system/zpoolpilot-collect-fast.timer
	usr/lib/systemd/system/zpoolpilot-collect-hardware.service
	usr/lib/systemd/system/zpoolpilot-collect-hardware.timer
	usr/lib/systemd/system/zpoolpilot-exporter.service
	usr/lib/sysusers.d/zpoolpilot.conf
	usr/lib/tmpfiles.d/zpoolpilot.conf
	usr/share/zpoolpilot/schemas/snapshot-v1.schema.json
	usr/share/zpoolpilot/schemas/action-plan-v1.schema.json
	usr/share/doc/zpoolpilot/testing.md
	usr/share/doc/zpoolpilot/development-plan.md
	usr/share/doc/zpoolpilot/LICENSE
	usr/share/man/man8/zpoolpilot.8
)
for relative in "${required[@]}"; do
	[[ -f "${stage}/${relative}" ]] || fail "missing staged file ${relative}"
done

[[ "$(stat -c '%a' "${stage}/usr/bin/zpoolpilot")" == 755 ]] || fail 'binary mode is not 0755'
[[ "$(stat -c '%a' "${stage}/etc/zpoolpilot/config.toml")" == 644 ]] || fail 'config mode is not 0644'
[[ "$(stat -c '%a' "${stage}/etc/zpoolpilot/bays.json")" == 644 ]] || fail 'bay mapping store mode is not 0644'
[[ "$(stat -c '%a' "${stage}/usr/lib/systemd/system/zpoolpilot-exporter.service")" == 644 ]] || fail 'unit mode is not 0644'

version_output="$("${stage}/usr/bin/zpoolpilot" --version)"
[[ "${version_output}" == zpoolpilot\ * ]] || fail 'staged binary did not execute its version command'

sentinel='administrator configuration must survive reinstall'
printf '%s\n' "${sentinel}" >"${stage}/etc/zpoolpilot/config.toml"
bay_sentinel='administrator bay mappings must survive reinstall'
printf '%s\n' "${bay_sentinel}" >"${stage}/etc/zpoolpilot/bays.json"
"${make_command}" --no-print-directory DESTDIR="${stage}" install >/dev/null
[[ "$(<"${stage}/etc/zpoolpilot/config.toml")" == "${sentinel}" ]] || fail 'reinstall overwrote existing config'
[[ "$(<"${stage}/etc/zpoolpilot/bays.json")" == "${bay_sentinel}" ]] || fail 'reinstall overwrote existing bay mappings'

printf '%s\n' 'install check: staged layout, modes, binary, config, and bay-map preservation passed'
