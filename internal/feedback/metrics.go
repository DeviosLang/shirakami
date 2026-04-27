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
	TokenUsage prometheus.Histogram

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
				Help:    "Distribution of token usage per analysis task.",
				Buckets: prometheus.ExponentialBuckets(100, 2, 12), // 100 → ~400k
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

// RecordTokenUsage records a single token-usage observation.
func (m *Metrics) RecordTokenUsage(tokens float64) {
	m.TokenUsage.Observe(tokens)
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
