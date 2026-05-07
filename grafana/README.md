# Grafana Dashboard 使用说明

## 1. 前提：Prometheus 已 scrape Pushgateway

在 `prometheus.yml` 加入：

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true          # 必须：保留 shirakami 推过来的 job/instance 标签
    scrape_interval: 30s
    static_configs:
      - targets: ['<pushgateway-addr>:8080']
```

改完后重载：
```bash
curl -X POST http://<prometheus-addr>:9090/-/reload
```

## 2. 导入 Dashboard

### 方式 A：粘贴 JSON（推荐）

1. 打开 Grafana → 左侧菜单 **Dashboards → Import**
2. 点击 **Upload JSON file**，选择 `grafana/dashboard.json`
3. 在 **Prometheus** 下拉框选择你的 Prometheus 数据源
4. 点击 **Import**

### 方式 B：API 导入（适合自动化）

```bash
curl -X POST http://<grafana-addr>:3000/api/dashboards/import \
  -H "Content-Type: application/json" \
  -u admin:admin \
  -d @- << 'EOF'
{
  "dashboard": $(cat grafana/dashboard.json),
  "overwrite": true,
  "folderId": 0,
  "inputs": [
    {
      "name": "DS_PROMETHEUS",
      "type": "datasource",
      "pluginId": "prometheus",
      "value": "Prometheus"
    }
  ]
}
EOF
```

## 3. Dashboard 面板说明

| 分区 | 面板 | 说明 |
|------|------|------|
| **任务概览** | 已完成/失败总数 | 累计计数，颜色告警 |
| | 误报率 | 来自用户 feedback，>15% 变红 |
| | 缓存命中率 | <30% 变红，>70% 绿 |
| | Checkpoint 恢复次数 | 崩溃恢复计数 |
| | 任务吞吐量 | 每分钟完成/失败速率时序图 |
| | Agent 步数分布 | P50/P95/P99 步数趋势 |
| **LLM Token 用量** | Token P50/P95 | 按 task_type（worker/triage/followup）分色 |
| | Token 消耗速率 | token/s，用于估算 API 费用 |
| **Worker 性能** | Worker 耗时 by tier | P0/P1/P2 优先级对比 |
| | Worker P95 by repo | 找出哪个 repo 分析最慢 |
| **Ghost Node** | rescued vs lost 计数 | 绿色=救活，红色=丢失 |
| | 救活率 Gauge | <50% 变红，>80% 绿 |
| | Ghost 速率趋势 | 每分钟发生量 |

## 4. 推荐告警规则（Grafana Alerting）

```yaml
# 任务失败率 > 20%（5 分钟窗口）
rate(shirakami_tasks_total{status="failed"}[5m]) /
rate(shirakami_tasks_total[5m]) > 0.2

# P95 Worker 耗时 > 300s
histogram_quantile(0.95, sum by (le) (rate(shirakami_worker_duration_seconds_bucket[10m]))) > 300

# Ghost Node 救活率 < 50%
sum(shirakami_ghost_nodes_total{outcome="rescued"}) /
(sum(shirakami_ghost_nodes_total{outcome="rescued"}) + sum(shirakami_ghost_nodes_total{outcome="lost"})) < 0.5
```
