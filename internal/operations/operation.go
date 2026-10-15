// Package operations owns the lifecycle of API-accepted change operations.
//
// Every mutating request that passes validation is assigned a stable
// operation ID before execution starts. Both execution lanes (Ansible
// playbooks and the in-process ops runner) append ordered, sequence-numbered
// steps to the same operation, which ends in exactly one terminal state:
// succeeded, failed, or timeout. The state is queryable by ID and
// subscribable (see broker.Journal) so browsers can isolate progress per
// request and resume after reconnects.
//
// History is bounded and in-memory only; see Config.
package operations

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"dumpstore/internal/ansible"
	"dumpstore/internal/broker"
)

// Status is the lifecycle state of an Operation.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusTimeout   Status = "timeout"
)

// Terminal reports whether s is an end state.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusTimeout:
		return true
	}
	return false
}

// Step is one executed command/task within an operation. Seq is unique and
// gapless within the operation starting at 1; Name/Status/Msg mirror
// ansible.TaskStep so the frontend op-log renders unchanged.
type Step struct {
	Seq    int64  `json:"seq"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Msg    string `json:"msg,omitempty"`
}

// Record is the serialisable, queryable state of an operation.
type Record struct {
	ID         string    `json:"operation_id"`
	Action     string    `json:"action"`
	Target     string    `json:"target,omitempty"`
	ReqID      string    `json:"req_id,omitempty"`
	Status     Status    `json:"status"`
	SeqFirst   int64     `json:"seq_first"`
	SeqLast    int64     `json:"seq_last"`
	Steps      []Step    `json:"steps"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Operation is a single accepted change with its step ring, terminal state
// and replay journal. Safe for concurrent use.
type Operation struct {
	mu         sync.Mutex
	rec        Record
	outcome    Status // strongest failure marked by execution lanes
	errMsg     string
	journal    *broker.Journal
	finishOnce sync.Once
	done       chan struct{}
	started    time.Time
}

// Snapshot returns an independent copy of the operation's current record.
func (o *Operation) Snapshot() Record {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := o.rec
	cp.Steps = append([]Step(nil), o.rec.Steps...)
	return cp
}

// Journal exposes the replay journal backing the operation's SSE stream.
func (o *Operation) Journal() *broker.Journal { return o.journal }

// Done is closed once the operation reaches its terminal state.
func (o *Operation) Done() <-chan struct{} { return o.done }

// ID returns the operation identifier.
func (o *Operation) ID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.rec.ID
}

// AddStep records one completed execution step, assigning the next gapless
// sequence number, truncating an oversized message and evicting the oldest
// retained steps beyond the ring cap. The step is also published live to the
// replay journal.
func (o *Operation) AddStep(maxSteps int, maxMsgBytes int, ts ansible.TaskStep) Step {
	o.mu.Lock()
	msg := truncateMsg(ts.Msg, maxMsgBytes)
	o.rec.SeqLast++
	step := Step{Seq: o.rec.SeqLast, Name: ts.Name, Status: ts.Status, Msg: msg}
	o.rec.Steps = append(o.rec.Steps, step)
	if len(o.rec.Steps) > maxSteps {
		drop := len(o.rec.Steps) - maxSteps
		copy(o.rec.Steps, o.rec.Steps[drop:])
		for i := len(o.rec.Steps) - drop; i < len(o.rec.Steps); i++ {
			o.rec.Steps[i] = Step{}
		}
		o.rec.Steps = o.rec.Steps[:len(o.rec.Steps)-drop]
	}
	o.rec.SeqFirst = o.rec.Steps[0].Seq
	journal := o.journal
	id := o.rec.ID
	o.mu.Unlock()

	slog.Debug("operation step", "op_id", id, "seq", step.Seq,
		"name", step.Name, "status", step.Status)
	if _, err := journal.Publish(step); err != nil {
		slog.Warn("operation: journal publish failed", "op_id", id, "seq", step.Seq, "err", err)
	}
	return step
}

// MarkFailure records a non-timeout execution failure. A previously marked
// timeout is not downgraded.
func (o *Operation) MarkFailure(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.outcome == "" {
		o.outcome = StatusFailed
		o.errMsg = msg
	}
}

// MarkTimeout records a timeout, which takes precedence over an ordinary
// failure even when multiple executions share one operation.
func (o *Operation) MarkTimeout(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcome = StatusTimeout
	o.errMsg = msg
}

// finish freezes the terminal state exactly once. httpStatus is the status
// written to the accepting HTTP response (used as a fallback when an
// execution lane forgot to mark an outcome). Returns true on the first call.
func (o *Operation) finish(httpStatus int, now time.Time) bool {
	finalized := false
	o.finishOnce.Do(func() {
		finalized = true
		o.mu.Lock()
		status := o.outcome
		if status == "" {
			if httpStatus >= 400 {
				status = StatusFailed
			} else {
				status = StatusSucceeded
			}
		}
		o.rec.Status = status
		o.rec.Error = o.errMsg
		o.rec.FinishedAt = now
		finishedAt := now
		errMsg := o.errMsg
		o.mu.Unlock()

		o.journal.Close()
		close(o.done)

		args := []any{
			"op_id", o.rec.ID, "status", status,
			"duration_ms", finishedAt.Sub(o.started).Milliseconds(),
			"failed", status != StatusSucceeded,
		}
		if errMsg != "" {
			args = append(args, "error", errMsg)
		}
		slog.Info("operation finished", args...)
	})
	return finalized
}

// truncateMsg caps msg to maxBytes (UTF-8 safe boundary), appending an
// ellipsis when something was dropped. Empty/short input is returned as-is.
func truncateMsg(msg string, maxBytes int) string {
	if len(msg) <= maxBytes {
		return msg
	}
	if maxBytes <= 4 {
		// Too small to keep any content plus the ellipsis.
		return "…"
	}
	cut := maxBytes - 3 // reserve space for "…" (3 bytes)
	for cut > 0 && !utf8RuneStart(msg[cut]) {
		cut--
	}
	if cut <= 0 {
		return "…"
	}
	return msg[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ErrAlready* classification helpers reused by the API layer.
var (
	// ErrInvalidID means a client-supplied operation ID failed the syntax check.
	ErrInvalidID = errors.New("invalid operation id")
	// ErrNotFound means the ID is syntactically valid but no live or retained
	// operation is known for it (and no tombstone records it either).
	ErrNotFound = errors.New("operation not found")
	// ErrExpired means the operation existed, finished and has since been
	// evicted from bounded history; its result is not recoverable.
	ErrExpired = errors.New("operation expired")
)
