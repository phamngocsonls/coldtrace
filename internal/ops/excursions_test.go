package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The HACCP excursion CSV is the "beyond the dashboard" story, and the
// brief is explicit that date operators carry an explicit timezone. The
// original pipeline did all its math in UTC and formatted nothing — so the
// audit export a food-safety inspector reads showed UTC instants, which in
// Vietnam are 7 hours off the wall clock an inspector works with. These
// tests pin the tz-aware read.

// TestExcursionsPipelineShape pins the excursion aggregation:
//   - $match FIRST (performance rule; also makes the window explicit)
//   - $setWindowFields/$shift for per-sample span credit
//   - $dateToString with the configured timezone for the inspector-facing
//     "last excursion seen at" column
func TestExcursionsPipelineShape(t *testing.T) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	p := ExcursionsPipeline(since, "Asia/Ho_Chi_Minh")

	if len(p) == 0 {
		t.Fatal("ExcursionsPipeline returned no stages")
	}
	first, ok := p[0].(bson.D)
	if !ok {
		t.Fatalf("first stage is %T, want bson.D", p[0])
	}
	if first[0].Key != "$match" {
		t.Fatalf("first stage is %s, want $match", first[0].Key)
	}
	matchDoc, ok := first[0].Value.(bson.M)
	if !ok {
		t.Fatalf("$match value is %T, want bson.M", first[0].Value)
	}
	if _, ok := matchDoc["ts"]; !ok {
		t.Error("$match must filter on ts")
	}
	if _, ok := matchDoc["tempC"]; !ok {
		t.Error("$match must filter on tempC (time + threshold narrow the scan)")
	}

	src := excursionsSprint(p)
	for _, want := range []string{"$setWindowFields", "$shift", "$group"} {
		if !excursionsContains(src, want) {
			t.Errorf("pipeline missing %s", want)
		}
	}
	if !excursionsContains(src, "$dateToString") {
		t.Error("pipeline has no $dateToString — the local-time audit column is the point")
	}
	if !excursionsContains(src, "Asia/Ho_Chi_Minh") {
		t.Error("$dateToString must carry Asia/Ho_Chi_Minh (brief: explicit tz on date operators)")
	}
	// A read/export pipeline must never write back.
	for _, st := range p {
		d, ok := st.(bson.D)
		if !ok {
			t.Fatalf("stage is %T, want bson.D", st)
		}
		switch d[0].Key {
		case "$merge", "$out":
			t.Errorf("export pipeline must not write: found %s", d[0].Key)
		}
	}
}

// TestExcursionsPipelineHonorsTimezoneArg guards the tz parameter: the
// operator must be able to render the audit in any zone without the
// pipeline silently hardcoding one.
func TestExcursionsPipelineHonorsTimezoneArg(t *testing.T) {
	p := ExcursionsPipeline(time.Now().UTC().Add(time.Hour), "Europe/Berlin")
	if !excursionsContains(excursionsSprint(p), "Europe/Berlin") {
		t.Error("timezone argument not carried into the pipeline")
	}
}

func excursionsSprint(v any) string { return fmt.Sprintf("%v", v) }
func excursionsContains(hay, needle string) bool {
	return strings.Contains(hay, needle)
}
