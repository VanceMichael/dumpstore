package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"dumpstore/internal/ansible"
	"dumpstore/internal/operations"
)

// OperationIDHeader carries the stable operation identifier: the browser
// sends a generated value on change requests (so a lost response still leaves
// it with a known ID) and the server echoes the accepted ID back.
const OperationIDHeader = "X-Operation-ID"

// opExpiredBody is the deterministic non-recoverable result returned for an
// operation that finished and has since been evicted from bounded history.
type opExpiredBody struct {
	Error       string `json:"error"`
	Recoverable bool   `json:"recoverable"`
	OperationID string `json:"operation_id"`
}

// opRunningBody answers an idempotent re-POST while the operation is still
// executing: the change is not run a second time.
type opRunningBody struct {
	Error       string `json:"error"`
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
}

// opReplayBody is the canonical body of an idempotent terminal replay. The
// original request's bespoke success body is not reconstructed; a retry that
// lost the original response needs exactly the terminal status, the error
// text (on failure) and the executed steps.
type opReplayBody struct {
	OperationID string             `json:"operation_id"`
	Status      operations.Status  `json:"status"`
	Error       string             `json:"error,omitempty"`
	Tasks       []ansible.TaskStep `json:"tasks,omitempty"`
}

// beginOperation accepts the change into the operation registry immediately
// before the first mutating execution. Call sites are expected to have
// already completed all input validation: a rejected request must never
// create an operation.
//
// It returns the (possibly wrapped) writer, a context-bearing request and a
// replayed flag. When replayed is true the response has already been written
// (400 malformed header, 409 same ID still running, 200/500 terminal replay,
// 410 expired) and the handler MUST return immediately without executing the
// change.
//
// Usage:
//
//	w, r, replayed := h.beginOperation(w, r, "user.create", name)
//	if replayed {
//	    return
//	}
func (h *Handler) beginOperation(w http.ResponseWriter, r *http.Request, action, target string) (http.ResponseWriter, *http.Request, bool) {
	op, outcome, err := h.opMgr.Start(
		r.Header.Get(OperationIDHeader),
		action, target, ReqIDFromContext(r.Context()),
	)
	if err != nil {
		switch {
		case errors.Is(err, operations.ErrInvalidID):
			writeError(r.Context(), w, http.StatusBadRequest, err, nil)
		case errors.Is(err, operations.ErrExpired):
			writeOperationExpired(w, r.Header.Get(OperationIDHeader))
		default:
			writeError(r.Context(), w, http.StatusInternalServerError, err, nil)
		}
		return w, r, true
	}

	w.Header().Set(OperationIDHeader, op.ID())

	if outcome != operations.StartCreated {
		h.writeReplay(w, op.Snapshot(), outcome)
		return w, r, true
	}

	r = r.WithContext(operations.WithContext(r.Context(), op))
	return &opResponseWriter{ResponseWriter: w, h: h, op: op}, r, false
}

// writeReplay renders the deterministic result of a re-POST with a known
// operation ID: 409 while still running, otherwise the frozen terminal
// outcome (200 success, 500 failure/timeout).
func (h *Handler) writeReplay(w http.ResponseWriter, rec operations.Record, outcome operations.StartOutcome) {
	w.Header().Set("Content-Type", "application/json")
	if outcome == operations.StartReplayRunning {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(opRunningBody{
			Error:       "operation already in progress",
			OperationID: rec.ID,
			Status:      string(operations.StatusRunning),
		})
		return
	}
	code := http.StatusOK
	if rec.Status != operations.StatusSucceeded {
		code = http.StatusInternalServerError
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(opReplayBody{
		OperationID: rec.ID,
		Status:      rec.Status,
		Error:       rec.Error,
		Tasks:       recordToTasks(rec),
	})
}

// recordToTasks converts the operation step ring to the TaskStep shape used
// by every existing op-log response.
func recordToTasks(rec operations.Record) []ansible.TaskStep {
	tasks := make([]ansible.TaskStep, 0, len(rec.Steps))
	for _, s := range rec.Steps {
		tasks = append(tasks, ansible.TaskStep{Name: s.Name, Status: s.Status, Msg: s.Msg})
	}
	return tasks
}

// writeOperationExpired answers 410 for a tombstoned (or re-POSTed expired)
// operation ID.
func writeOperationExpired(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(opExpiredBody{
		Error:       "operation expired: its result is no longer available",
		Recoverable: false,
		OperationID: id,
	})
}

// opResponseWriter finalises the operation the first time the handler commits
// its response — including when the write fails because the client has
// disconnected: the mutation keeps running (WithoutCancel in the runners)
// and the terminal state still becomes queryable.
type opResponseWriter struct {
	http.ResponseWriter
	h    *Handler
	op   *operations.Operation
	once sync.Once
}

func (w *opResponseWriter) finalize(code int) {
	w.once.Do(func() { w.h.opMgr.Complete(w.op, code) })
}

func (w *opResponseWriter) WriteHeader(code int) {
	w.finalize(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *opResponseWriter) Write(b []byte) (int, error) {
	// An implicit 200 happens when the handler writes without WriteHeader.
	w.finalize(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when supported.
func (w *opResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
