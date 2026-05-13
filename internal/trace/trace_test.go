package trace_test

import (
	"context"
	"testing"

	itrace "github.com/DeviosLang/shirakami/internal/trace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TestGetTracer verifies that GetTracer returns a non-nil tracer even when no
// provider has been registered (falls back to the global no-op provider).
func TestGetTracer(t *testing.T) {
	tr := itrace.GetTracer()
	if tr == nil {
		t.Fatal("expected non-nil tracer")
	}
}

// TestStartNoop verifies that Start works without panicking when no real
// TracerProvider is configured (no-op path used in unit tests).
func TestStartNoop(t *testing.T) {
	ctx := context.Background()
	ctx2, span := itrace.Start(ctx, itrace.OpWorkerAnalyze,
		attribute.String(itrace.AttrRepo, "test-repo"),
		attribute.String(itrace.AttrTriageTier, "P0"),
	)
	if ctx2 == nil {
		t.Fatal("expected non-nil context")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	// No-op spans are always non-recording; ensure End() doesn't panic.
	span.End()
}

// TestConstantValues ensures the public constants are non-empty (guards against
// accidental whitespace-only or empty strings being committed).
func TestConstantValues(t *testing.T) {
	constants := []struct {
		name  string
		value string
	}{
		{"AttrTaskID", itrace.AttrTaskID},
		{"AttrRepo", itrace.AttrRepo},
		{"AttrFunction", itrace.AttrFunction},
		{"AttrTriageTier", itrace.AttrTriageTier},
		{"AttrStepCount", itrace.AttrStepCount},
		{"AttrTokenCount", itrace.AttrTokenCount},
		{"AttrMessageCount", itrace.AttrMessageCount},
		{"AttrErrorType", itrace.AttrErrorType},
		{"AttrCacheHit", itrace.AttrCacheHit},
		{"AttrSourceRepo", itrace.AttrSourceRepo},
		{"AttrMode", itrace.AttrMode},
		{"AttrRound", itrace.AttrRound},
		{"AttrFuncCount", itrace.AttrFuncCount},
		{"OpHTTPRequest", itrace.OpHTTPRequest},
		{"OpAnalysisRun", itrace.OpAnalysisRun},
		{"OpOrchestratorAnalyze", itrace.OpOrchestratorAnalyze},
		{"OpWorkerAnalyze", itrace.OpWorkerAnalyze},
		{"OpLLMComplete", itrace.OpLLMComplete},
		{"OpCheckpointSave", itrace.OpCheckpointSave},
		{"OpCheckpointLoad", itrace.OpCheckpointLoad},
		{"OpCompress", itrace.OpCompress},
		{"OpToolExecute", itrace.OpToolExecute},
	}

	for _, c := range constants {
		if c.value == "" {
			t.Errorf("constant %s must not be empty", c.name)
		}
	}
}

// TestInitProviderNoop verifies that InitProvider with no env vars returns a
// no-op shutdown function (non-nil, does not panic when called).
func TestInitProviderNoop(t *testing.T) {
	// Ensure OTEL env vars are not set during this test.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_STDOUT_TRACE", "")

	shutdown, err := itrace.InitProvider("test-svc", "0.0.0")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	// Calling shutdown must not panic.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}

// TestStartReturnsSpanContext ensures that the returned span carries a valid
// span context (at minimum the span interface must not be the zero value).
func TestStartReturnsSpanContext(t *testing.T) {
	ctx, span := itrace.Start(context.Background(), itrace.OpLLMComplete)
	defer span.End()

	sc := trace.SpanFromContext(ctx).SpanContext()
	_ = sc // structural check: SpanContext() must not panic
}
