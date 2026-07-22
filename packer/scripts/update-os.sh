#!/usr/bin/env bash
set -euo pipefail

if ((EUID != 0)); then
  echo "${0} must run as root" >&2
  exit 1
fi

apt-get -o DPkg::Lock::Timeout=300 update
apt-get \
  -o DPkg::Lock::Timeout=300 \
  -o Dpkg::Options::=--force-confdef \
  -o Dpkg::Options::=--force-confold \
  --assume-yes \
  full-upgrade
