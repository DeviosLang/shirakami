package feedback

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus metrics for the shirakami service.
type Metrics struct {
	// TasksTotal counts completed tasks by status label.
	TasksTotal *prometheus.CounterVec

	// TokenUsage tracks the distribution of token consumption per task.
	// Deprecated: prefer LLMTokensTotal for per-step token recording.
	TokenUsage prometheus.Histogram

	// LLMTokensTotal is a histogram of token counts per LLM step, labelled by
	// model name and task_type (e.g. "worker", "triage", "followup").
	// Buckets cover typical request sizes from 1 k to 256 k tokens.
	LLMTokensTotal *prometheus.HistogramVec

	// GhostNodeTotal counts ghost-node outcomes labelled by result:
	//   "rescued" — the function was found at a different path via ripgrep rescue.
	//   "lost"    — the function could not be located and was dropped from results.
	GhostNodeTotal *prometheus.CounterVec

	// WorkerDuration is a histogram of single-Worker analysis duration in seconds,
	// labelled by triage_tier (P0/P1/P2/default) and repo name.
	WorkerDuration *prometheus.HistogramVec

	// CheckpointResumed counts how many AgentLoop runs were resumed from a
	// previously saved checkpoint rather than started fresh.
	CheckpointResumed prometheus.Counter

	// CacheHitRatio is a gauge representing the current cache hit rate (0–1).
	CacheHitRatio prometheus.Gauge

	// FalsePositiveRate is a gauge representing the current false-positive rate
	// derived from user feedback (0–1).
	FalsePositiveRate prometheus.Gauge

	// StepsHistogram tracks the distribution of analysis step counts per task.
	StepsHistogram prometheus.Histogram
}

// NewMetrics registers all Prometheus metrics and returns a Metrics instance.
// All metrics use promauto so they are registered on the default registry.
func NewMetrics() *Metrics {
	return &Metrics{
		TasksTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "shirakami_tasks_total",
				Help: "Total number of analysis tasks by completion status.",
			},
			[]string{"status"},
		),

		TokenUsage: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "shirakami_token_usage_histogram",
				Help:    "Distribution of token usage per analysis task (legacy; prefer shirakami_llm_tokens).",
				Buckets: prometheus.ExponentialBuckets(100, 2, 12), // 100 → ~400k
			},
		),

		LLMTokensTotal: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "shirakami_llm_tokens",
				Help: "Token count per LLM call, labelled by model and task_type.",
				// 512 → ~131k, covers most single-step prompt+completion sizes.
				Buckets: prometheus.ExponentialBuckets(512, 2, 9), // 512, 1k, 2k … 131k
			},
			[]string{"model", "task_type"},
		),

		GhostNodeTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "shirakami_ghost_nodes_total",
				Help: "Count of ghost-node outcomes: rescued (path corrected) or lost (dropped).",
			},
			[]string{"outcome"}, // "rescued" | "lost"
		),

		WorkerDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "shirakami_worker_duration_seconds",
				Help: "Worker analysis wall-clock duration in seconds, by triage tier and repo.",
				// 1 s → ~512 s (≈8.5 min). Workers typically run 10–300 s.
				Buckets: prometheus.ExponentialBuckets(1, 2, 10),
			},
			[]string{"tier", "repo"},
		),

		CheckpointResumed: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "shirakami_checkpoint_resumed_total",
				Help: "Number of AgentLoop runs resumed from a saved checkpoint.",
			},
		),

		CacheHitRatio: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "shirakami_cache_hit_ratio",
				Help: "Current cache hit ratio (0–1).",
			},
		),

		FalsePositiveRate: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "shirakami_false_positive_rate",
				Help: "Current false-positive rate derived from user feedback (0–1).",
			},
		),

		StepsHistogram: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "shirakami_steps_histogram",
				Help:    "Distribution of agent loop step counts per analysis.",
				Buckets: prometheus.LinearBuckets(1, 5, 20), // 1, 6, 11, … 96
			},
		),
	}
}

// Handler returns the Prometheus metrics HTTP handler to be mounted at /metrics.
func Handler() http.Handler {
	return promhttp.Handler()
}

// RecordTask increments the task counter for the given status
// (e.g. "completed", "failed").
func (m *Metrics) RecordTask(status string) {
	m.TasksTotal.WithLabelValues(status).Inc()
}

// RecordTokenUsage records a single token-usage observation (legacy helper).
func (m *Metrics) RecordTokenUsage(tokens float64) {
	m.TokenUsage.Observe(tokens)
}

// RecordLLMTokens records a per-step LLM token count for the given model and task type.
// model should be the LLM model name (e.g. "gpt-4o"); taskType is a short label
// such as "worker", "triage", or "followup".
func (m *Metrics) RecordLLMTokens(model, taskType string, totalTokens int) {
	m.LLMTokensTotal.WithLabelValues(model, taskType).Observe(float64(totalTokens))
}

// RecordGhostNode increments the ghost-node outcome counter.
// outcome must be "rescued" or "lost".
func (m *Metrics) RecordGhostNode(outcome string) {
	m.GhostNodeTotal.WithLabelValues(outcome).Inc()
}

// RecordWorkerDuration records a single Worker run duration for the given tier and repo.
func (m *Metrics) RecordWorkerDuration(tier, repo string, seconds float64) {
	m.WorkerDuration.WithLabelValues(tier, repo).Observe(seconds)
}

// RecordCheckpointResumed increments the checkpoint-resume counter.
func (m *Metrics) RecordCheckpointResumed() {
	m.CheckpointResumed.Inc()
}

// RecordSteps records the number of agent loop steps for one analysis.
func (m *Metrics) RecordSteps(steps float64) {
	m.StepsHistogram.Observe(steps)
}

// SetCacheHitRatio updates the cache-hit-ratio gauge.
func (m *Metrics) SetCacheHitRatio(ratio float64) {
	m.CacheHitRatio.Set(ratio)
}

// SetFalsePositiveRate updates the false-positive-rate gauge.
func (m *Metrics) SetFalsePositiveRate(rate float64) {
	m.FalsePositiveRate.Set(rate)
}
