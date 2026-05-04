package logger

import (
	"sync"

	"go.uber.org/zap"
)

// New creates a zap logger. mode should be "development" or "production".
func New(mode string) (*zap.Logger, error) {
	if mode == "development" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
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
