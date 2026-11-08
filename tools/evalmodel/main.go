// evalmodel — CLI for offline model accuracy evaluation (see
// internal/modeleval for the shared logic; ops-api exposes the same report
// at GET /api/model/eval).
//
// usage: go run ./tools/evalmodel [--n 220]
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/onemount/coldtrace/internal/modeleval"
)

func main() {
	nPerClass := flag.Int("n", 220, "evaluation windows per class")
	flag.Parse()

	rep := modeleval.Evaluate(*nPerClass)
	fmt.Printf("windows: %d (%d per class)  accuracy: %.2f%%\n\n",
		rep.Windows, *nPerClass, rep.Accuracy*100)

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "class\tprecision\trecall\tf1\tsupport")
	for _, c := range rep.PerClass {
		fmt.Fprintf(w, "%s\t%.3f\t%.3f\t%.3f\t%d\n", c.Class, c.Precision, c.Recall, c.F1, c.Support)
	}
	w.Flush()
}
