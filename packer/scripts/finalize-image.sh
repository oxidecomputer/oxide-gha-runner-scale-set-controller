#!/usr/bin/env bash
set -euo pipefail

if ((EUID != 0)); then
  echo "${0} must run as root" >&2
  exit 1
fi

: "${BUILD_USER:?BUILD_USER must be set}"

build_home="$(getent passwd "${BUILD_USER}" | cut -d: -f6)"
if [[ -z "${build_home}" ]]; then
  echo "Unable to find the home directory for ${BUILD_USER}" >&2
  exit 1
fi

# Remove package caches and indexes.
apt-get clean
rm -rf /var/lib/apt/lists/*

# Remove build-time access and host identity.
rm -f "${build_home}/.ssh/authorized_keys"
rm -f "${build_home}/.ssh/authorized_keys2"
rm -f /root/.ssh/authorized_keys /root/.ssh/authorized_keys2
rm -f /etc/ssh/ssh_host_*

# Remove system identity and entropy persisted by systemd.
rm -f /boot/loader/random-seed
rm -f /etc/hostname
rm -f /etc/machine-info
rm -f /var/lib/systemd/credential.secret

# Make the next boot a cloud-init first boot and generate a new machine ID.
cloud-init clean --logs --machine-id
systemctl stop systemd-random-seed.service || true
rm -f /var/lib/systemd/random-seed

# Remove build logs, histories, and temporary files without truncating active
# binary systemd journal files.
journalctl --rotate || true
journalctl --vacuum-time=1s || true
find /var/log -type f ! -path '/var/log/journal/*' -exec truncate -s 0 {} +
rm -f /root/.bash_history "${build_home}/.bash_history"
find /tmp /var/tmp -mindepth 1 -delete

# The Oxide builder's API stop is forceful. Sync writes.
sync
