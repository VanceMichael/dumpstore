// Package ops executes single-command write operations in-process.
//
// It replaces Ansible for mutations that are just "assert inputs, run one
// argv command" (zpool scrub/replace/add/…, zfs snapshot/destroy/set) —
// see issue #115. Input validation lives in the API handlers; commands run
// argv-style with no shell. Step results use the ansible.TaskStep shape and
// are appended to the request's tracked internal/operations record (gapless
// sequence, single terminal state, per-operation resumable SSE), so the
// frontend op-log dialog renders this lane and the Ansible lane identically.
//
// Ansible remains the write path for config-file ownership (smb.conf),
// service state, and OS resources (users/groups); long-running data-plane
// transfers stay on internal/jobs.
package ops

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"dumpstore/internal/ansible"
)

// tracer resolves through the global provider — a no-op unless the OTEL SDK
// is installed at startup.
var tracer = otel.Tracer("dumpstore")

// ErrTimedOut wraps the error returned when a step's command does not finish
// within the runner's per-step timeout. Callers classify the operation
// terminal state with errors.Is(err, ErrTimedOut).
var ErrTimedOut = errors.New("ops step timed out")

// allowedBinaries is the closed set of binaries the ops layer may execute.
// Handlers build steps from hardcoded argv literals; this check makes
// binary substitution structurally impossible even if a future call site
// accidentally plumbs user input into Argv[0]. Argument values are
// validated by the handlers (name/size/device regexes), and argv execution
// without a shell rules out shell metacharacter injection.
var allowedBinaries = map[string]bool{"zfs": true, "zpool": true}

// Step is one command in an operation: Argv is executed without a shell
// and reported in the op-log under Name.
type Step struct {
	Name string
	Argv []string
	// ContinueOnError lets the run proceed past a failure of this step
	// (used by batch operations); Run still returns an aggregate error.
	ContinueOnError bool
}

// Result holds the executed steps in the same shape as a playbook run.
type Result struct {
	steps []ansible.TaskStep
}

// Steps returns the ordered step results.
func (r *Result) Steps() []ansible.TaskStep { return r.steps }

// NewResult builds a result from steps. It exists so non-runner code (tests,
// fakes) can construct a result value.
func NewResult(steps []ansible.TaskStep) *Result { return &Result{steps: steps} }

// Runner executes Steps sequentially with a per-step timeout.
type Runner struct {
	Timeout time.Duration
	metrics *opsMetrics
}

// NewRunner returns a Runner with the default 5-minute per-step timeout.
func NewRunner() *Runner {
	return &Runner{Timeout: 5 * time.Minute, metrics: newOpsMetrics()}
}

// Run executes the steps in order. Each completed step is passed to onStep
// (if non-nil) as it finishes. Execution stops at the first failing step
// unless that step has ContinueOnError set; the returned Result always
// contains every step that ran, so callers can hand it to the op-log even
// on error.
func (r *Runner) Run(ctx context.Context, steps []Step, onStep func(ansible.TaskStep)) (*Result, error) {
	res := &Result{}
	var failed []string
	var failedErrs []error
	for _, st := range steps {
		ts, err := r.runStep(ctx, st)
		res.steps = append(res.steps, ts)
		if onStep != nil {
			onStep(ts)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%s)", st.Name, ts.Msg))
			failedErrs = append(failedErrs, err)
			if !st.ContinueOnError {
				break
			}
		}
	}
	if len(failed) > 0 {
		// Wrap the underlying step errors (joined) in addition to the human
		// summary so errors.Is(err, ErrTimedOut) still reaches the cause.
		return res, fmt.Errorf("%d of %d step(s) failed: %s: %w",
			len(failed), len(res.steps), strings.Join(failed, "; "), errors.Join(failedErrs...))
	}
	return res, nil
}

func (r *Runner) runStep(ctx context.Context, st Step) (ansible.TaskStep, error) {
	if len(st.Argv) == 0 || !allowedBinaries[st.Argv[0]] {
		ts := ansible.TaskStep{Name: st.Name, Status: "failed",
			Msg: "refusing to execute: binary not in the ops allowlist"}
		return ts, fmt.Errorf("%s: %s", st.Name, ts.Msg)
	}

	// Argv never carries secrets (validated names/sizes/devices only).
	ctx, span := tracer.Start(ctx, "ops."+opLabel(st.Argv), trace.WithAttributes(
		attribute.String("ops.step", st.Name),
		attribute.StringSlice("ops.argv", st.Argv),
	))
	defer span.End()

	// ctx parents the span; cancellation is not propagated — an interrupted
	// client must not kill a mutating zfs/zpool command mid-flight.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Timeout)
	defer cancel()

	start := time.Now()
	out, err := exec.CommandContext(ctx, st.Argv[0], st.Argv[1:]...).CombinedOutput()
	r.metrics.record(opLabel(st.Argv), time.Since(start), err != nil)

	// Timeout takes precedence over the generic exit error so callers can
	// classify the operation terminal state via errors.Is(err, ErrTimedOut).
	if ctx.Err() == context.DeadlineExceeded {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = fmt.Sprintf("timed out after %s", r.Timeout)
		}
		ts := ansible.TaskStep{Name: st.Name, Status: "failed", Msg: msg}
		stepErr := fmt.Errorf("%w: %s: step %q after %s",
			ErrTimedOut, strings.Join(st.Argv, " "), st.Name, r.Timeout)
		span.RecordError(stepErr)
		span.SetStatus(codes.Error, msg)
		return ts, stepErr
	}

	msg := strings.TrimSpace(string(out))
	ts := ansible.TaskStep{Name: st.Name, Status: "changed", Msg: msg}
	if err != nil {
		ts.Status = "failed"
		if ts.Msg == "" {
			ts.Msg = err.Error()
		}
		stepErr := fmt.Errorf("%s: %s", strings.Join(st.Argv, " "), ts.Msg)
		span.RecordError(stepErr)
		span.SetStatus(codes.Error, ts.Msg)
		return ts, stepErr
	}
	return ts, nil
}

// opLabel derives the metric label from an argv: the command plus its
// subcommand ("zpool scrub", "zfs destroy"), keeping label cardinality low.
func opLabel(argv []string) string {
	if len(argv) >= 2 {
		return argv[0] + " " + argv[1]
	}
	return strings.Join(argv, " ")
}
