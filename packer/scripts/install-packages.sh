#!/usr/bin/env bash
set -euo pipefail

if ((EUID != 0)); then
  echo "${0} must run as root" >&2
  exit 1
fi

packages=(
  bash
  build-essential
  bzip2
  ca-certificates
  cloud-init
  curl
  file
  git
  git-lfs
  gnupg
  gzip
  jq
  locales
  openssh-client
  pkg-config
  rsync
  sudo
  tar
  tzdata
  unzip
  xz-utils
  zip
  zstd
)

apt-get \
  -o DPkg::Lock::Timeout=300 \
  --assume-yes \
  --no-install-recommends \
  install "${packages[@]}"
