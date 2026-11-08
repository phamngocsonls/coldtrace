package ai

import (
	"context"
	"log/slog"
	"math/rand"
)

// testLogger discards debug output during tests.
func testLogger() *slog.Logger {
	return slog.New(discardHandler{})
}

type discardHandler struct{}

func (discardHandler) Enabled(_ context.Context, _ slog.Level) bool  { return false }
func (discardHandler) Handle(_ context.Context, _ slog.Record) error { return nil }
func (h discardHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return h }
func (h discardHandler) WithGroup(_ string) slog.Handler             { return h }

func newTestRng() *rand.Rand { return rand.New(rand.NewSource(7)) }
