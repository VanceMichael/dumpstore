package operations

import (
	"fmt"
	"io"
)

// EmitMetrics writes operation counters/gauges in Prometheus text exposition
// format to w. The status label cardinality is fixed to the three terminal
// states.
func (m *Manager) EmitMetrics(w io.Writer) {
	accepted := m.AcceptedCount()
	finished := m.FinishedCount()
	running := m.RunningCount()

	fmt.Fprintf(w,
		"# HELP dumpstore_operations_accepted_total Total change operations accepted by the API.\n"+
			"# TYPE dumpstore_operations_accepted_total counter\n"+
			"dumpstore_operations_accepted_total %d\n", accepted)

	fmt.Fprint(w,
		"# HELP dumpstore_operations_finished_total Operations finalised, by terminal status.\n"+
			"# TYPE dumpstore_operations_finished_total counter\n")
	for _, s := range []Status{StatusSucceeded, StatusFailed, StatusTimeout} {
		fmt.Fprintf(w, "dumpstore_operations_finished_total{status=%q} %d\n", s, finished[s])
	}

	fmt.Fprintf(w,
		"# HELP dumpstore_operations_running Operations currently executing.\n"+
			"# TYPE dumpstore_operations_running gauge\n"+
			"dumpstore_operations_running %d\n", running)
}
