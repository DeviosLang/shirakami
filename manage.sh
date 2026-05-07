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
#   持续监控单个任务状态（支持完整 ID 或前缀）：
#     bash manage.sh watch <task_id_or_prefix>
#
#   查看任务详情：
#     bash manage.sh task <task_id>
#
#   查看任务结果（模式可选 e2e / chain / ut / all）：
#     bash manage.sh e2e   <task_id>   # E2E 测试场景 + 入口点
#     bash manage.sh chain <task_id>   # 调用链 + 入口点
#     bash manage.sh ut    <task_id>   # UT 建议
#     bash manage.sh all   <task_id>   # 完整结果（原始 JSON）

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
              .id,
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
              .id,
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

  # ── watch（持续轮询单个任务）────────────────────────────────────────────────
  watch)
    PREFIX=${2:-""}
    if [[ -z "$PREFIX" ]]; then
      echo "用法：bash manage.sh watch <task_id_or_prefix>" >&2
      exit 1
    fi
    POLL=${POLL_INTERVAL:-5}

    # 如果传入的是前缀，从任务列表中找完整 ID
    TASK_ID="$PREFIX"
    if [[ ${#PREFIX} -lt 36 ]]; then
      MATCHED=$(curl -s "$API" | jq -r --arg p "$PREFIX" '.[] | select(.id | startswith($p)) | .id' | head -1)
      if [[ -z "$MATCHED" ]]; then
        echo "错误：找不到前缀为 ${PREFIX} 的任务" >&2
        exit 1
      fi
      TASK_ID="$MATCHED"
      echo "→ 匹配到任务：$TASK_ID"
    fi

    echo "⏳ 持续监控任务（每 ${POLL}s 刷新，Ctrl-C 退出）..."
    echo ""
    last_status=""
    while true; do
      RESP=$(curl -s "${API}/${TASK_ID}" 2>/dev/null || true)
      if [[ -z "$RESP" ]]; then
        echo "[$(date '+%H:%M:%S')] 请求失败，重试..."
        sleep "$POLL"
        continue
      fi
      STATUS=$(echo "$RESP" | jq -r '.status // "unknown"')
      PROGRESS=$(echo "$RESP" | jq -r '.progress // ""')
      TS=$(date '+%H:%M:%S')

      if [[ "$STATUS" != "$last_status" || -n "$PROGRESS" ]]; then
        echo -n "[$TS] status=${STATUS}"
        [[ -n "$PROGRESS" ]] && echo -n "  progress=${PROGRESS}"
        echo ""
        last_status=$STATUS
      fi

      if [[ "$STATUS" == "completed" || "$STATUS" == "failed" ]]; then
        echo ""
        if [[ "$STATUS" == "completed" ]]; then
          echo "✅ 完成。查看结果："
          echo "  bash manage.sh e2e   $TASK_ID"
          echo "  bash manage.sh chain $TASK_ID"
          echo "  bash manage.sh ut    $TASK_ID"
        else
          ERR=$(echo "$RESP" | jq -r '.error // .error_message // "unknown error"')
          echo "❌ 失败：$ERR"
        fi
        break
      fi

      sleep "$POLL"
    done
    ;;

  # ── 结果视图：e2e / chain / ut / all ────────────────────────────────────────
  e2e|chain|ut|all)
    TASK_ID=${2:-""}
    if [[ -z "$TASK_ID" ]]; then
      echo "用法：bash manage.sh ${CMD} <task_id>" >&2
      exit 1
    fi

    # all → 无子路径，其他 → /{mode}
    if [[ "$CMD" == "all" ]]; then
      URL="${API}/${TASK_ID}"
    else
      URL="${API}/${TASK_ID}/${CMD}"
    fi

    RESP=$(curl -s "$URL")

    # ── e2e ──────────────────────────────────────────────────────────────────
    if [[ "$CMD" == "e2e" ]]; then
      IMPACT=$(echo "$RESP" | jq -r '.impact_summary // ""')
      if [[ -n "$IMPACT" ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  E2E 测试场景（impact_summary）"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$IMPACT"
        echo ""
      fi

      EP_COUNT=$(echo "$RESP" | jq '.entry_points | length')
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  Entry Points（共 ${EP_COUNT} 个）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$RESP" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
      echo ""

      WARNINGS=$(echo "$RESP" | jq -r '.warnings[]? // empty')
      if [[ -n "$WARNINGS" ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  ⚠️  分析警告"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$WARNINGS" | sed 's/^/  /'
        echo ""
      fi

    # ── chain ─────────────────────────────────────────────────────────────────
    elif [[ "$CMD" == "chain" ]]; then
      EP_COUNT=$(echo "$RESP" | jq '.entry_points | length')
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  Entry Points（共 ${EP_COUNT} 个）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$RESP" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
      echo ""

      NODE_COUNT=$(echo "$RESP" | jq '.call_chain | length')
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  调用链节点（共 ${NODE_COUNT} 个）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$RESP" | jq -r '.call_chain[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
      echo ""

    # ── ut ────────────────────────────────────────────────────────────────────
    elif [[ "$CMD" == "ut" ]]; then
      UT=$(echo "$RESP" | jq -r '.ut_suggestions // ""')
      if [[ -n "$UT" && "$UT" != "null" ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  UT 建议"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$UT"
        echo ""
      else
        echo "（暂无 UT 建议）"
      fi

      FA_COUNT=$(echo "$RESP" | jq '.function_analyses | length // 0')
      if [[ "$FA_COUNT" -gt 0 ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  函数分析（共 ${FA_COUNT} 个）"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$RESP" | jq -r '.function_analyses[]? | "  \(.entry_function // .function // "—")  scenarios=\(.entry_scenarios | length)"'
        echo ""
      fi

    # ── all ───────────────────────────────────────────────────────────────────
    else
      echo "$RESP" | jq .
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
