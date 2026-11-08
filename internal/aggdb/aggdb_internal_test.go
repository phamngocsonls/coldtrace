package aggdb

import (
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// slopeWindowMinutes pulls the `range` lower bound out of the slope
// $derivative window definition.
func slopeWindowMinutes(t *testing.T, setWindow interface{}) float64 {
	t.Helper()
	doc, ok := setWindow.(bson.D)
	if !ok {
		t.Fatalf("$setWindowFields value is %T, want bson.D", setWindow)
	}
	for _, f := range doc {
		if f.Key != "output" {
			continue
		}
		output, ok := f.Value.(bson.D)
		if !ok {
			t.Fatalf("output is %T, want bson.D", f.Value)
		}
		for _, o := range output {
			if o.Key != "slope" {
				continue
			}
			spec, ok := o.Value.(bson.D)
			if !ok {
				t.Fatalf("slope spec is %T, want bson.D", o.Value)
			}
			for _, s := range spec {
				if s.Key != "window" {
					continue
				}
				win, ok := s.Value.(bson.D)
				if !ok {
					t.Fatalf("window is %T, want bson.D", s.Value)
				}
				for _, w := range win {
					if w.Key != "range" {
						continue
					}
					bounds, ok := w.Value.(bson.A)
					if !ok || len(bounds) != 2 {
						t.Fatalf("window range is %v, want a 2-element bson.A", w.Value)
					}
					// range is {lowerOffset, 0}; the span is the magnitude.
					switch v := bounds[0].(type) {
					case int:
						return float64(-v)
					case int32:
						return float64(-v)
					case int64:
						return float64(-v)
					case float64:
						return -v
					}
					t.Fatalf("window lower bound is %T", bounds[0])
				}
			}
		}
	}
	t.Fatal("no slope window definition found")
	return 0
}

func sprint(v interface{}) string { return fmt.Sprintf("%v", v) }

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
