// Tests for the slog → OTel bridge: severity mapping and attribute
// flattening. Emission correctness against a real provider is covered by
// the exporter pipeline end-to-end (logs verified in Datadog).
package otlp

import (
	"log/slog"
	"testing"
)

func TestSeverityText(t *testing.T) {
	cases := []struct {
		lvl  slog.Level
		want string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
		{slog.LevelError + 4, "ERROR"},
	}
	for _, c := range cases {
		if got := severityText(c.lvl); got != c.want {
			t.Fatalf("severityText(%v) = %q, want %q", c.lvl, got, c.want)
		}
	}
}

func TestSlogAttrToKV(t *testing.T) {
	kv := slogAttrToKV(slog.String("s", "v"))
	if kv.Key != "s" || kv.Value.AsString() != "v" {
		t.Fatalf("string attr = %+v", kv)
	}
	iv := slogAttrToKV(slog.Int("i", 7))
	if iv.Value.AsInt64() != 7 {
		t.Fatalf("int attr = %+v", iv)
	}
	bv := slogAttrToKV(slog.Bool("b", true))
	if !bv.Value.AsBool() {
		t.Fatalf("bool attr = %+v", bv)
	}
	fv := slogAttrToKV(slog.Float64("f", 1.5))
	if fv.Value.AsFloat64() != 1.5 {
		t.Fatalf("float attr = %+v", fv)
	}
	dv := slogAttrToKV(slog.Duration("d", 1500000000))
	if dv.Value.AsString() != "1.5s" {
		t.Fatalf("duration attr = %+v", dv)
	}
}
