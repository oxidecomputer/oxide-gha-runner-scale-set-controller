#!/usr/bin/env bash
set -euo pipefail

if ((EUID != 0)); then
  echo "${0} must run as root" >&2
  exit 1
fi

: "${GITHUB_ACTIONS_RUNNER_VERSION:?must be set}"
: "${GITHUB_ACTIONS_RUNNER_SHA256SUM:?must be set}"
: "${GITHUB_ACTIONS_RUNNER_SERVICE_SOURCE:?must be set}"
: "${GITHUB_ACTIONS_RUNNER_PATH_SOURCE:?must be set}"
: "${GITHUB_ACTIONS_RUNNER_TMPFILES_SOURCE:?must be set}"
: "${GITHUB_ACTIONS_RUNNER_LAUNCHER_SOURCE:?must be set}"

if [[ "$(uname -m)" != "x86_64" ]]; then
  echo "GitHub Actions runner images require x86_64" >&2
  exit 1
fi

if ! id github-actions-runner >/dev/null 2>&1; then
  useradd \
    --create-home \
    --home-dir /home/github-actions-runner \
    --shell /bin/bash \
    github-actions-runner
fi

install_dir="/opt/github-actions-runner"
if [[ -e "${install_dir}" ]]; then
  echo "Installation already exists at ${install_dir}" >&2
  exit 1
fi

archive="actions-runner-linux-x64-${GITHUB_ACTIONS_RUNNER_VERSION}.tar.gz"
url="https://github.com/actions/runner/releases/download/"
url+="v${GITHUB_ACTIONS_RUNNER_VERSION}/${archive}"
download_dir="$(mktemp --directory)"
trap 'rm -rf "${download_dir}"' EXIT

curl \
  --fail \
  --location \
  --retry 5 \
  --output "${download_dir}/${archive}" \
  "${url}"
printf '%s  %s\n' \
  "${GITHUB_ACTIONS_RUNNER_SHA256SUM}" \
  "${download_dir}/${archive}" | sha256sum --check --status

install --directory --owner=root --group=root --mode=0755 "${install_dir}"
tar \
  --extract \
  --gzip \
  --file "${download_dir}/${archive}" \
  --directory "${install_dir}"

(
  cd "${install_dir}"
  ./bin/installdependencies.sh
)

test -x "${install_dir}/run.sh"
test -x "${install_dir}/bin/Runner.Listener"
chown -R github-actions-runner:github-actions-runner "${install_dir}"

install --directory --owner=root --group=root --mode=0755 \
  /usr/local/libexec
install \
  --owner=root \
  --group=root \
  --mode=0755 \
  "${GITHUB_ACTIONS_RUNNER_LAUNCHER_SOURCE}" \
  /usr/local/libexec/github-actions-runner
install \
  --owner=root \
  --group=root \
  --mode=0644 \
  "${GITHUB_ACTIONS_RUNNER_SERVICE_SOURCE}" \
  /etc/systemd/system/github-actions-runner.service
install \
  --owner=root \
  --group=root \
  --mode=0644 \
  "${GITHUB_ACTIONS_RUNNER_PATH_SOURCE}" \
  /etc/systemd/system/github-actions-runner.path
install \
  --owner=root \
  --group=root \
  --mode=0644 \
  "${GITHUB_ACTIONS_RUNNER_TMPFILES_SOURCE}" \
  /etc/tmpfiles.d/github-actions-runner.conf

systemctl daemon-reload
systemd-analyze verify \
  /etc/systemd/system/github-actions-runner.service \
  /etc/systemd/system/github-actions-runner.path
systemd-tmpfiles --create /etc/tmpfiles.d/github-actions-runner.conf
systemctl enable github-actions-runner.path
