#!/usr/bin/env bash
# Shirakami one-shot deploy + run + fetch-report.
#
# Usage:
#   scripts/deploy.sh [--mr <label>] [--desc "<description>"] [--build] [--no-wait]
#
# Defaults:
#   --mr     compute-mr1681
#   --desc   "shirakami analysis <JOB_ID>"
#   --build  skip docker build (reuse existing :latest image)
#   --no-wait skip waiting for Job completion
#
# Job is named: shirakami-analyze-<MR>-<JOB_ID>, where JOB_ID is
# YYYYMMDD-HHMMSS. Logs are saved to reports/<JOB_ID>.md on completion.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

# ── Defaults ────────────────────────────────────────────────────────────────
JOB_MR="compute-mr1681"
JOB_DESC=""
DO_BUILD=0
NO_WAIT=0
NAMESPACE="rag-etl"
IMAGE="mirrors.tencent.com/cvm/shirakami:latest"

# ── Parse args ──────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mr)       JOB_MR="$2"; shift 2 ;;
    --desc)     JOB_DESC="$2"; shift 2 ;;
    --build)    DO_BUILD=1; shift ;;
    --no-wait)  NO_WAIT=1; shift ;;
    -h|--help)
      sed -n '2,16p' "$0" | sed 's/^# \?//'
      exit 0 ;;
    *)
      echo "Unknown arg: $1" >&2; exit 2 ;;
  esac
done

# ── JOB_ID = UTC timestamp ──────────────────────────────────────────────────
JOB_ID="$(date -u +%Y%m%d-%H%M%S)"
if [[ -z "$JOB_DESC" ]]; then
  JOB_DESC="shirakami analysis ${JOB_MR} ${JOB_ID}"
fi
export JOB_ID JOB_MR JOB_DESC

JOB_NAME="shirakami-analyze-${JOB_MR}-${JOB_ID}"
REPORT_PATH="${ROOT_DIR}/reports/${JOB_ID}.md"

echo "▶ Job name:    ${JOB_NAME}"
echo "▶ Namespace:   ${NAMESPACE}"
echo "▶ Description: ${JOB_DESC}"
echo "▶ Report path: ${REPORT_PATH}"

# ── Build + push (optional) ─────────────────────────────────────────────────
if [[ "$DO_BUILD" == "1" ]]; then
  echo "▶ Building image ${IMAGE}..."
  docker build -t "$IMAGE" . | tail -3
  echo "▶ Pushing image ${IMAGE}..."
  docker push "$IMAGE" | tail -3
fi

# ── Render job.yaml via envsubst and apply ──────────────────────────────────
if ! command -v envsubst >/dev/null 2>&1; then
  echo "ERROR: envsubst is required (install 'gettext-base' or use perl)" >&2
  exit 3
fi

echo "▶ Applying Job manifest..."
envsubst '${JOB_ID} ${JOB_MR} ${JOB_DESC}' < k8s/job.yaml | kubectl apply -f -

# ── Wait + fetch logs ───────────────────────────────────────────────────────
if [[ "$NO_WAIT" == "1" ]]; then
  echo "▶ --no-wait: skipping wait. Watch with:"
  echo "    kubectl -n ${NAMESPACE} logs -f job/${JOB_NAME}"
  exit 0
fi

echo "▶ Waiting for Job to complete (max 2h)..."
if ! kubectl -n "$NAMESPACE" wait --for=condition=complete \
    "job/${JOB_NAME}" --timeout=7200s 2>&1 | tail -1; then
  echo "⚠ Wait ended without success; pulling current logs anyway."
fi

mkdir -p "$(dirname "$REPORT_PATH")"
echo "▶ Saving logs to ${REPORT_PATH}..."
kubectl -n "$NAMESPACE" logs "job/${JOB_NAME}" > "$REPORT_PATH" 2>&1 || true

# ── Summary ─────────────────────────────────────────────────────────────────
echo
echo "─── Summary ───────────────────────────────────────────────────────────"
grep -E "^## |变更函数数量|直接影响|跨仓影响" "$REPORT_PATH" 2>/dev/null | head -12 || true
echo
echo "✔ Full report: ${REPORT_PATH}"
