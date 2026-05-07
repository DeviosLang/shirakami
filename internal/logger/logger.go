package logger

import (
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// beijingTimeEncoder formats log timestamps as RFC3339 in Asia/Shanghai (CST+8).
// Falls back to UTC if the timezone cannot be loaded (e.g. in a minimal container
// without tzdata — unlikely since the Dockerfile copies /usr/share/zoneinfo).
func beijingTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.UTC
	}
	enc.AppendString(t.In(loc).Format("2006-01-02T15:04:05.000+08:00"))
}

// New creates a zap logger. mode should be "development" or "production".
func New(mode string) (*zap.Logger, error) {
	if mode == "development" {
		return zap.NewDevelopment()
	}
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.EncodeTime = beijingTimeEncoder
	return cfg.Build()
}

// Must creates a logger and panics on error.
func Must(mode string) *zap.Logger {
	l, err := New(mode)
	if err != nil {
		panic(err)
	}
	return l
}

// ---------------------------------------------------------------------------
// Global default logger — used by agent/worker/triage packages that don't
// want to thread a *zap.Logger through every constructor.
// ---------------------------------------------------------------------------

var (
	defaultLogger *zap.Logger
	defaultOnce   sync.Once
)

// SetDefault installs the given logger as the package-level default.
// Call this from main() after constructing the primary logger.
func SetDefault(l *zap.Logger) {
	defaultLogger = l
}

// Default returns the global default logger.
// If not yet initialised, lazily creates a production logger.
func Default() *zap.Logger {
	if defaultLogger == nil {
		defaultOnce.Do(func() {
			if defaultLogger == nil {
				defaultLogger = Must("production")
			}
		})
	}
	return defaultLogger
}

// S returns the sugared variant of the default logger for convenience.
func S() *zap.SugaredLogger {
	return Default().Sugar()
}
