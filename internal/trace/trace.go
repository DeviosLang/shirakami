// Package trace provides shared OpenTelemetry tracer helpers and constant
// attribute/operation names for Shirakami's distributed tracing instrumentation.
//
// Usage:
//
//	ctx, span := trace.GetTracer().Start(ctx, trace.OpWorkerAnalyze)
//	span.SetAttributes(attribute.String(trace.AttrRepo, task.RepoName))
//	defer span.End()
//
// TracerProvider initialisation is done in cmd/server/main.go via InitProvider.
// When no provider has been configured (e.g. in tests) the global no-op provider
// is used automatically — all Span operations become cheap no-ops.
package trace

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/DeviosLang/shirakami"

// ── Span attribute keys ───────────────────────────────────────────────────────

const (
	// AttrTaskID is the unique analysis task identifier.
	AttrTaskID = "task.id"
	// AttrRepo is the repository short-name (matches repos[].name in config).
	AttrRepo = "repo.name"
	// AttrFunction is the changed/traced function name.
	AttrFunction = "function.name"
	// AttrTriageTier is the triage priority tier: "P0", "P1", or "P2".
	AttrTriageTier = "triage.tier"
	// AttrStepCount is the number of agent-loop steps consumed.
	AttrStepCount = "step.count"
	// AttrTokenCount is the cumulative token count for a span.
	AttrTokenCount = "token.count"
	// AttrMessageCount is the number of LLM messages in the context at call time.
	AttrMessageCount = "message.count"
	// AttrErrorType is a short error classification tag (e.g. "llm_failed", "parse_error").
	AttrErrorType = "error.type"
	// AttrCacheHit records whether the result was served from cache.
	AttrCacheHit = "cache.hit"
	// AttrSourceRepo is the repository that originated the diff.
	AttrSourceRepo = "source_repo"
	// AttrMode is the analysis mode ("deep" or "fast").
	AttrMode = "analysis.mode"
	// AttrRound is the cross-repo iteration round number.
	AttrRound = "analysis.round"
	// AttrFuncCount is how many changed functions were extracted from the diff.
	AttrFuncCount = "function.count"
)

// ── Operation names ───────────────────────────────────────────────────────────

const (
	// OpHTTPRequest wraps an incoming HTTP handler invocation.
	OpHTTPRequest = "http.request"
	// OpAnalysisRun wraps the top-level runAnalysis goroutine in cmd/server.
	OpAnalysisRun = "analysis.run"
	// OpOrchestratorAnalyze wraps Orchestrator.Run.
	OpOrchestratorAnalyze = "orchestrator.analyze"
	// OpWorkerAnalyze wraps WorkerAgent.Analyse per repository.
	OpWorkerAnalyze = "worker.analyze"
	// OpLLMComplete wraps a single LLM API call inside the agent loop.
	OpLLMComplete = "llm.complete"
	// OpCheckpointSave wraps a checkpoint persistence write.
	OpCheckpointSave = "checkpoint.save"
	// OpCheckpointLoad wraps a checkpoint restore read.
	OpCheckpointLoad = "checkpoint.load"
	// OpCompress wraps a token-budget compression pass.
	OpCompress = "compress"
	// OpToolExecute wraps a single tool call inside the agent loop.
	OpToolExecute = "tool.execute"
)

// GetTracer returns the package-scoped tracer from the global OTel provider.
// If no provider has been registered (e.g. in unit tests) this returns the
// global no-op tracer — all span operations are cheap stubs.
func GetTracer() oteltrace.Tracer {
	return otel.Tracer(tracerName)
}

// Start is a convenience wrapper around GetTracer().Start that returns the
// child context and span together.
//
//	ctx, span := trace.Start(ctx, trace.OpWorkerAnalyze,
//	    attribute.String(trace.AttrRepo, "payment-service"))
//	defer span.End()
func Start(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return GetTracer().Start(ctx, op, oteltrace.WithAttributes(attrs...))
}

// ── Provider initialisation ───────────────────────────────────────────────────

// InitProvider sets up the global OTel TracerProvider and returns a shutdown
// function that the caller must invoke (typically via defer in main).
//
// Exporter selection:
//   - OTEL_EXPORTER_OTLP_ENDPOINT set → OTLP/HTTP exporter (Jaeger/Tempo)
//   - OTEL_STDOUT_TRACE=true           → stdout exporter (local debugging)
//   - neither                           → no-op (SDK not installed, nothing exported)
//
// The returned shutdown function flushes pending spans; pass it a context with
// a reasonable deadline (e.g. context.WithTimeout 5s).
func InitProvider(serviceName, version string) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		return noop, nil // resource failure is non-fatal
	}

	var exporter sdktrace.SpanExporter

	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		exporter, err = otlptracehttp.New(context.Background())
		if err != nil {
			return noop, nil // exporter init failure is non-fatal
		}
	} else if os.Getenv("OTEL_STDOUT_TRACE") == "true" {
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return noop, nil
		}
	} else {
		// No exporter configured — leave the global no-op provider in place.
		return noop, nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

func noop(_ context.Context) error { return nil }
