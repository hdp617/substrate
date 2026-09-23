#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Install the micro-VM (kata + cloud-hypervisor) sandbox assets into the
# cluster's object store bucket: assembles the asset set (assemble.sh; skipped
# if OUT already has them) and stages it under kata-assets/ (rustfs on kind,
# GCS on GKE), where atelet fetches it.
#
# hack/install-ate.sh --deploy-ate-system applies the `microvm` SandboxConfig
# that names these assets and runs this script by default, so call it directly
# only to (re-)stage into a cluster installed with --skip-microvm-assets.
#
# Every asset sha256 is pinned in the SandboxConfig, so a staged set that does
# not match the pins is rejected by atelet at fetch time rather than booting a
# VM on unexpected bytes.
#
# Like the other hack scripts, this sources .ate-dev-env.sh for the cluster /
# registry / bucket settings unless NO_DEV_ENV is set.
#
# Env (most come from .ate-dev-env.sh):
#   BUCKET_NAME      object store bucket for assets/snapshots (default: ate-snapshots).
#   KUBECTL_CONTEXT  (optional) kube context; threaded into kubectl.
#   PROJECT_ID       (optional) GCP project for the GCS asset upload (GKE path).
#   ARCH             target arch (default: from KO_DEFAULTPLATFORMS, else host arch).
#   OUT              asset dir (default: $PWD/bin/microvm-assets/$ARCH, gitignored).
#   ATE_INSTALL_KIND "true" for the kind path (stage assets to rustfs); default
#                    false uploads assets to GCS.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

# Source the environment (cluster, registry, bucket) like the other hack scripts;
# callers set NO_DEV_ENV to skip this and use kind defaults.
if [[ -r .ate-dev-env.sh ]] && [[ -z "${NO_DEV_ENV:-}" ]]; then
  source .ate-dev-env.sh
fi

BUCKET_NAME="${BUCKET_NAME:-ate-snapshots}"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-}"
ATE_INSTALL_KIND="${ATE_INSTALL_KIND:-false}"

usage() {
  cat <<EOF
Usage: $0

Assembles the micro-VM asset set and stages it into the cluster's object store
bucket under kata-assets/.

Options:
  -h, --help  Show this message.
EOF
}

if [[ $# -gt 1 ]]; then
  echo "Error: unexpected argument $2" >&2
  usage
  exit 1
fi
case "${1:-}" in
  "") ;;
  -h|--help) usage; exit 0 ;;
  *) echo "Error: unknown argument $1" >&2; usage; exit 1 ;;
esac

# The kind path stages through the in-cluster rustfs, so it needs a cluster to
# talk to. kubectl falls back to localhost:8080 when neither --context nor a
# kubeconfig current-context is set, which would surface as a confusing
# "connection refused" only after the assets have already been assembled.
# Resolve the target cluster up front instead.
if [[ "${ATE_INSTALL_KIND}" == "true" ]] &&
  [[ -z "${KUBECTL_CONTEXT}" ]] && ! kubectl config current-context >/dev/null 2>&1; then
  echo "Error: no kube context to target: KUBECTL_CONTEXT is empty and the" >&2
  echo "       kubeconfig has no current-context." >&2
  echo "       Set KUBECTL_CONTEXT (e.g. in .ate-dev-env.sh) or run:" >&2
  echo "         kubectl config use-context <name>" >&2
  exit 1
fi

# ANSI color codes for prettier output (mirrors hack/install-ate.sh).
COLOR_CYAN='\033[1;36m'
COLOR_RESET='\033[0m'
log() {
  echo -e "${COLOR_CYAN}[install-microvm-deps]: $*${COLOR_RESET}"
}

# Target arch: match the images' platform (KO_DEFAULTPLATFORMS is set by
# .ate-dev-env.sh on GKE and by the kind wrapper); fall back to the host arch.
if [[ -z "${ARCH:-}" ]]; then
  if [[ -n "${KO_DEFAULTPLATFORMS:-}" ]]; then
    ARCH="${KO_DEFAULTPLATFORMS##*/}"
  else
    ARCH="$(go env GOARCH)"
  fi
fi
OUT="${OUT:-${ROOT}/bin/microvm-assets/$ARCH}"

# --- 1. assets: assemble (if missing or stale) -----------------------------
need_assemble=false
for f in cloud-hypervisor virtiofsd vmlinux rootfs.img; do
  if [[ ! -f "${OUT}/${f}" ]]; then
    need_assemble=true
    break
  fi
done
# Presence alone is not enough. The filenames don't change when a version pin
# moves, so an asset dir assembled before a bump looks complete while holding the old
# bytes — we'd then stage those against a SandboxConfig pinning the new shas, and the
# mismatch would only surface at runtime as an actor wedged in STATUS_RESUMING while
# atelet rejects the download. Compare the stamp assemble.sh left against the one it
# would write now; a dir predating the stamp, or left by a failed assemble (which clears
# the stamp before overwriting anything), has no file and re-assembles.
if [[ "${need_assemble}" == "false" ]]; then
  want_stamp="$(ARCH="${ARCH}" hack/microvm-assets/assemble.sh --print-stamp)"
  if [[ "$(cat "${OUT}/.asset-versions" 2>/dev/null)" != "${want_stamp}" ]]; then
    log "Asset set in ${OUT} is stale (version stamp mismatch); re-assembling."
    need_assemble=true
  fi
fi
if [[ "${need_assemble}" == "true" ]]; then
  log "Assembling micro-VM assets into ${OUT} (ARCH=${ARCH})..."
  ARCH="${ARCH}" OUT="${OUT}" hack/microvm-assets/assemble.sh
else
  log "Assets already present in ${OUT}; skipping assemble."
fi

# --- 2. stage assets to rustfs (kind) / GCS (GKE) --------------------------
# Upload the four assets under kata-assets/, where atelet fetches them: the
# in-cluster rustfs (S3 API) on kind, or the GCS bucket on GKE.
if [[ "${ATE_INSTALL_KIND}" == "true" ]]; then
  log "Staging assets to in-cluster rustfs bucket ${BUCKET_NAME} (kata-assets/)..."
  OUT="${OUT}" BUCKET="${BUCKET_NAME}" KUBECTL_CONTEXT="${KUBECTL_CONTEXT}" hack/microvm-assets/stage-to-rustfs.sh
else
  log "Uploading assets to gs://${BUCKET_NAME}/kata-assets/ ..."
  OUT="${OUT}" BUCKET="${BUCKET_NAME}" hack/microvm-assets/stage-to-gcs.sh
fi

log "Done. ActorTemplates reach these assets through the cluster-wide microvm SandboxConfig (sandboxConfig.configName: microvm)."
