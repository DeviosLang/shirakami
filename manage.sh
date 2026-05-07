#!/bin/bash
# Shirakami 管理脚本
#
# 用法：
#
#   查看任务队列（最近 20 条）：
#     bash manage.sh tasks
#
#   只看排队/运行中的任务：
#     bash manage.sh tasks pending
#     bash manage.sh tasks running
#
#   清理全部缓存：
#     bash manage.sh cache clear
#
#   清理指定任务的缓存（下次相同 diff 会重新分析）：
#     bash manage.sh cache clear <task_id>
#
#   查看任务详情：
#     bash manage.sh task <task_id>
#
#   查看任务 e2e 结果：
#     bash manage.sh e2e <task_id>

set -euo pipefail

API_BASE=${API_BASE:-"http://43.137.205.156:8080"}
API="${API_BASE}/api/v1/tasks"
CACHE_API="${API_BASE}/api/v1/cache"

_usage() {
  grep '^#' "$0" | grep -v '^#!/' | sed 's/^# \{0,2\}//'
  exit 0
}

CMD=${1:-""}

case "$CMD" in
  # ── tasks ──────────────────────────────────────────────────────────────────
  tasks)
    FILTER=${2:-""}
    echo "→ 查询任务列表..."
    RESP=$(curl -s "$API")
    if [[ -z "$RESP" || "$RESP" == "null" ]]; then
      echo "（无任务或服务不可达）"
      exit 0
    fi

    if [[ -n "$FILTER" ]]; then
      echo "$RESP" | jq -r --arg f "$FILTER" '
        ["ID", "状态", "进度", "类型", "仓库", "创建时间"],
        (
          .[]
          | select(.status == $f)
          | [
              .id[:8],
              .status,
              (.progress // "—"),
              (.input_type // "—"),
              (.source_repo // "—"),
              (.created_at // "—" | split("T")[0])
            ]
        )
        | @tsv' | column -t -s $'\t'
    else
      echo "$RESP" | jq -r '
        ["ID", "状态", "进度", "类型", "仓库", "创建时间"],
        (
          .[]
          | [
              .id[:8],
              .status,
              (.progress // "—"),
              (.input_type // "—"),
              (.source_repo // "—"),
              (.created_at // "—" | split("T")[0])
            ]
        )
        | @tsv' | column -t -s $'\t'
    fi

    # 统计摘要
    echo ""
    echo "$RESP" | jq -r '
      "共 \(length) 条  " +
      "pending=\(map(select(.status=="pending")) | length)  " +
      "running=\(map(select(.status=="running")) | length)  " +
      "completed=\(map(select(.status=="completed")) | length)  " +
      "failed=\(map(select(.status=="failed")) | length)"'
    ;;

  # ── task（单条详情）─────────────────────────────────────────────────────────
  task)
    TASK_ID=${2:-""}
    if [[ -z "$TASK_ID" ]]; then
      echo "用法：bash manage.sh task <task_id>" >&2
      exit 1
    fi
    curl -s "${API}/${TASK_ID}" | jq .
    ;;

  # ── e2e 结果 ────────────────────────────────────────────────────────────────
  e2e)
    TASK_ID=${2:-""}
    if [[ -z "$TASK_ID" ]]; then
      echo "用法：bash manage.sh e2e <task_id>" >&2
      exit 1
    fi
    E2E=$(curl -s "${API}/${TASK_ID}/e2e")

    IMPACT=$(echo "$E2E" | jq -r '.impact_summary // ""')
    if [[ -n "$IMPACT" ]]; then
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  E2E 测试场景（impact_summary）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$IMPACT"
      echo ""
    fi

    EP_COUNT=$(echo "$E2E" | jq '.entry_points | length')
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  Entry Points（共 ${EP_COUNT} 个）"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "$E2E" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
    echo ""

    WARNINGS=$(echo "$E2E" | jq -r '.warnings[]? // empty')
    if [[ -n "$WARNINGS" ]]; then
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  ⚠️  分析警告"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$WARNINGS" | sed 's/^/  /'
    fi
    ;;

  # ── cache ───────────────────────────────────────────────────────────────────
  cache)
    SUBCMD=${2:-""}
    TASK_ID=${3:-""}

    if [[ "$SUBCMD" != "clear" ]]; then
      echo "用法：" >&2
      echo "  bash manage.sh cache clear            # 清理全部缓存" >&2
      echo "  bash manage.sh cache clear <task_id>  # 清理指定任务缓存" >&2
      exit 1
    fi

    if [[ -n "$TASK_ID" ]]; then
      echo "→ 清理任务 ${TASK_ID} 的缓存..."
      RESP=$(curl -s -X DELETE "${API}/${TASK_ID}/cache")
      DELETED=$(echo "$RESP" | jq -r '.deleted // 0')
      MSG=$(echo "$RESP" | jq -r '.message // ""')
      if [[ -n "$MSG" ]]; then
        echo "  $MSG"
      else
        echo "  ✓ 已清理（deleted=${DELETED}）"
      fi
    else
      echo "→ 清理全部缓存..."
      RESP=$(curl -s -X DELETE "$CACHE_API")
      DELETED=$(echo "$RESP" | jq -r '.deleted // 0')
      echo "  ✓ 已清理 ${DELETED} 条缓存记录"
    fi
    ;;

  # ── help ────────────────────────────────────────────────────────────────────
  help|--help|-h|"")
    _usage
    ;;

  *)
    echo "未知命令：$CMD" >&2
    echo "运行 bash manage.sh help 查看用法" >&2
    exit 1
    ;;
esac
