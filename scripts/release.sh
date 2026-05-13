#!/usr/bin/env bash
# scripts/release.sh — Build, push, and rolling-restart the shirakami-server.
#
# Usage:
#   scripts/release.sh [OPTIONS]
#
# Options:
#   --image  IMAGE    Full image reference (default: mirrors.tencent.com/cvm/shirakami:latest)
#   --ns     NS       Kubernetes namespace   (default: rag-etl)
#   --deploy DEPLOY   Deployment name        (default: shirakami-server)
#   --no-push         Skip docker push (useful when the node pulls from local daemon)
#   --no-restart      Skip kubectl rollout restart (build+push only)
#   --timeout SECS    Seconds to wait for rollout (default: 300)
#   -h, --help        Show this help
#
# Exit codes:
#   0  success
#   1  rollout failed / timed out
#   2  bad arguments
#   3  required tool missing

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
IMAGE="mirrors.tencent.com/cvm/shirakami:latest"
NAMESPACE="rag-etl"
DEPLOYMENT="shirakami-server"
DO_PUSH=1
DO_RESTART=1
ROLLOUT_TIMEOUT=300

# ── Helpers ──────────────────────────────────────────────────────────────────
info()  { echo "▶ $*"; }
ok()    { echo "✔ $*"; }
fail()  { echo "✖ $*" >&2; exit 1; }

# ── Parse args ───────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)    IMAGE="$2";            shift 2 ;;
    --ns)       NAMESPACE="$2";        shift 2 ;;
    --deploy)   DEPLOYMENT="$2";       shift 2 ;;
    --no-push)  DO_PUSH=0;             shift   ;;
    --no-restart) DO_RESTART=0;        shift   ;;
    --timeout)  ROLLOUT_TIMEOUT="$2";  shift 2 ;;
    -h|--help)
      sed -n '2,20p' "$0" | sed 's/^# \?//'
      exit 0 ;;
    *)
      echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

# ── Prerequisite checks ───────────────────────────────────────────────────────
for cmd in docker kubectl; do
  command -v "$cmd" >/dev/null 2>&1 || { echo "ERROR: '$cmd' not found in PATH" >&2; exit 3; }
done

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

BUILD_START=$(date +%s)
echo
echo "════════════════════════════════════════════════════"
echo "  Shirakami release  $(date '+%Y-%m-%d %H:%M:%S')"
echo "  Image:      ${IMAGE}"
echo "  Namespace:  ${NAMESPACE}"
echo "  Deployment: ${DEPLOYMENT}"
echo "════════════════════════════════════════════════════"
echo

# ── Step 1: docker build ──────────────────────────────────────────────────────
info "Building Docker image: ${IMAGE}"
docker build -t "${IMAGE}" .
ok "Build complete"
echo

# ── Step 2: docker push ───────────────────────────────────────────────────────
if [[ "$DO_PUSH" == "1" ]]; then
  info "Pushing image to registry..."
  docker push "${IMAGE}"
  ok "Push complete"
  echo
else
  info "--no-push: skipping docker push"
  echo
fi

# ── Step 3: kubectl rollout restart ──────────────────────────────────────────
if [[ "$DO_RESTART" == "1" ]]; then
  info "Triggering rolling restart: deployment/${DEPLOYMENT} in namespace ${NAMESPACE}"
  kubectl -n "${NAMESPACE}" rollout restart "deployment/${DEPLOYMENT}"
  echo

  # Poll readyReplicas instead of using `kubectl rollout status` (which relies
  # on watch/bookmark events that may hang on some API Server configurations).
  info "Waiting for rollout to complete (polling every 5s, timeout: ${ROLLOUT_TIMEOUT}s)..."
  DESIRED=$(kubectl -n "${NAMESPACE}" get deployment "${DEPLOYMENT}" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
  DEADLINE=$(( $(date +%s) + ROLLOUT_TIMEOUT ))
  ROLLOUT_OK=0
  while [[ $(date +%s) -lt $DEADLINE ]]; do
    READY=$(kubectl -n "${NAMESPACE}" get deployment "${DEPLOYMENT}" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    UPDATED=$(kubectl -n "${NAMESPACE}" get deployment "${DEPLOYMENT}" \
      -o jsonpath='{.status.updatedReplicas}' 2>/dev/null || echo "0")
    AVAILABLE=$(kubectl -n "${NAMESPACE}" get deployment "${DEPLOYMENT}" \
      -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo "0")
    printf "\r  ready=%s  updated=%s  available=%s  desired=%s   " \
      "${READY:-0}" "${UPDATED:-0}" "${AVAILABLE:-0}" "${DESIRED}"
    if [[ "${UPDATED:-0}" == "${DESIRED}" ]] && \
       [[ "${READY:-0}"   == "${DESIRED}" ]] && \
       [[ "${AVAILABLE:-0}" == "${DESIRED}" ]]; then
      ROLLOUT_OK=1
      break
    fi
    sleep 5
  done
  echo  # newline after the \r progress line

  if [[ "$ROLLOUT_OK" == "1" ]]; then
    ok "Rollout succeeded (desired=${DESIRED}, all replicas ready)"
  else
    fail "Rollout did not complete within ${ROLLOUT_TIMEOUT}s. Check with:
    kubectl -n ${NAMESPACE} get pods -l app=${DEPLOYMENT}
    kubectl -n ${NAMESPACE} describe deployment/${DEPLOYMENT}"
  fi
  echo

  # Show current pod state for quick sanity check.
  info "Current pods:"
  kubectl -n "${NAMESPACE}" get pods \
    -l "app=${DEPLOYMENT}" \
    --sort-by='.metadata.creationTimestamp' 2>/dev/null || true
  echo
else
  info "--no-restart: skipping kubectl rollout restart"
  echo
fi

# ── Done ─────────────────────────────────────────────────────────────────────
BUILD_END=$(date +%s)
ELAPSED=$(( BUILD_END - BUILD_START ))
ok "Release done in ${ELAPSED}s"
