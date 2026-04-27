package logger

import (
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
