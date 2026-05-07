package feedback

import (
	"context"
	"fmt"
	"time"

	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/prometheus/client_golang/prometheus/push"
)

// Pusher wraps a Prometheus Pushgateway client and provides:
//   - An immediate push on startup (connectivity check).
//   - Periodic background pushes at a configurable interval.
//   - A single Push() call for use after each task completes.
//
// All push failures are logged as warnings and never returned as errors —
// metric loss is non-fatal and should not interrupt analysis work.
type Pusher struct {
	p        *push.Pusher
	interval time.Duration
	stopCh   chan struct{}
}

// NewPusher creates and validates a Pusher against the given Pushgateway URL.
//
//   - url      — Pushgateway address, e.g. "http://21.215.89.245:8080"
//   - jobName  — Prometheus job label, e.g. "shirakami"
//   - interval — how often to push in the background; 0 disables periodic push
//
// Returns an error only when the initial connectivity push fails (so callers
// know the address is unreachable at startup and can warn the operator).
func NewPusher(url, jobName string, interval time.Duration) (*Pusher, error) {
	log := logger.S()

	p := push.New(url, jobName)

	// Validate connectivity with an immediate push.
	if err := p.Push(); err != nil {
		return nil, fmt.Errorf("pushgateway initial push to %s failed: %w", url, err)
	}
	log.Infow("metrics.pushgateway_connected",
		"url", url,
		"job", jobName,
		"interval_s", int(interval.Seconds()),
	)

	return &Pusher{
		p:        p,
		interval: interval,
		stopCh:   make(chan struct{}),
	}, nil
}

// Start launches a background goroutine that pushes metrics every p.interval.
// It returns immediately; call Stop() to shut it down.
// If interval == 0 the goroutine exits immediately (only task-triggered pushes occur).
func (pu *Pusher) Start() {
	if pu.interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(pu.interval)
		defer ticker.Stop()
		log := logger.S()
		for {
			select {
			case <-ticker.C:
				if err := pu.p.Push(); err != nil {
					log.Warnw("metrics.push_failed", "err", err.Error())
				} else {
					log.Debugw("metrics.push_ok", "interval_s", int(pu.interval.Seconds()))
				}
			case <-pu.stopCh:
				return
			}
		}
	}()
}

// Push does a single synchronous push to the Pushgateway.
// Call this after each task completes to keep the gateway current without
// waiting for the next periodic tick.
// Errors are logged as warnings; the call never panics.
func (pu *Pusher) Push() {
	log := logger.S()
	if err := pu.p.Push(); err != nil {
		log.Warnw("metrics.push_failed", "err", err.Error())
	}
}

// Stop signals the background goroutine to exit and does a final push.
// Safe to call multiple times or when Start() was never called.
func (pu *Pusher) Stop(ctx context.Context) {
	select {
	case <-pu.stopCh:
		// already stopped
	default:
		close(pu.stopCh)
	}
	// Final push to make sure the last state is captured.
	log := logger.S()
	if err := pu.p.Push(); err != nil {
		log.Warnw("metrics.final_push_failed", "err", err.Error())
	} else {
		log.Infow("metrics.final_push_ok")
	}
}
