package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dumpstore/internal/operations"
)

// getOperation handles GET /api/operations/{id}: the deterministic query
// point for a change accepted earlier — including one whose accepting HTTP
// connection was lost.
func (h *Handler) getOperation(w http.ResponseWriter, r *http.Request) {
	op, err := h.opMgr.Get(r.PathValue("id"))
	if err != nil {
		h.writeOperationLookupError(w, r, err)
		return
	}
	writeJSON(r.Context(), w, op.Snapshot())
}

// getOperationEvents handles GET /api/operations/{id}/events:
//
//	Last-Event-ID header or ?since=<n> selects the resume position.
//	Position before the retained window (or 0) → one op.snapshot full replace.
//	Position inside the window → only newer op.step events, then op.finished.
//	A slow consumer whose buffer fills is dropped by the journal; the
//	response ends and EventSource reconnects with Last-Event-ID, replaying
//	the gap along the same path.
func (h *Handler) getOperationEvents(w http.ResponseWriter, r *http.Request) {
	op, err := h.opMgr.Get(r.PathValue("id"))
	if err != nil {
		h.writeOperationLookupError(w, r, err)
		return
	}

	since, ok := parseResumePosition(w, r)
	if !ok {
		return // 400 already written
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(r.Context(), w, http.StatusInternalServerError,
			errors.New("streaming not supported by this transport"), nil)
		return
	}

	// Subscribe BEFORE snapshotting so no step can fall into the gap
	// between the two reads (see snapshot-mode gating below).
	journal := op.Journal()
	initial, ch, _ := journal.Subscribe(since)
	defer journal.Unsubscribe(ch)
	rec := op.Snapshot()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// The replay window is the Journal's, not the record ring's: the journal
	// can byte-evict earlier than the 500-step ring fills. A position older
	// than its oldest retained event (and every fresh subscriber at 0)
	// converges via a full op.snapshot; a position inside the journal window
	// receives only the missing op.step events.
	snapshotMode := since == 0 || since < journal.OldestSeq()
	if snapshotMode {
		writeSSE(flusher, w, rec.SeqLast, "op.snapshot", rec)
		if rec.Status.Terminal() {
			// Snapshot already carries the terminal record; the browser
			// closes the stream here. Release the server side as well.
			return
		}
	} else {
		for _, ev := range initial {
			var step operations.Step
			if err := json.Unmarshal(ev.Data, &step); err != nil {
				continue
			}
			writeSSE(flusher, w, ev.Seq, "op.step", step)
		}
		if rec.Status.Terminal() {
			writeSSE(flusher, w, rec.SeqLast, "op.finished", finishedPayload(rec))
			return
		}
	}

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	done := op.Done()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				// Journal dropped this slow subscriber. Ending the response
				// makes EventSource reconnect with Last-Event-ID; replay
				// closes the gap.
				return
			}
			var step operations.Step
			if err := json.Unmarshal(ev.Data, &step); err != nil {
				continue
			}
			// Snapshot mode may already contain steps that were published
			// between Subscribe and Snapshot; never send those twice.
			if snapshotMode && step.Seq <= rec.SeqLast {
				continue
			}
			writeSSE(flusher, w, ev.Seq, "op.step", step)
		case <-done:
			// select may choose a ready done while tail steps are still
			// queued in ch; flush them before finished so a pure-SSE client
			// that resumes at the finished id has not lost any step.
			for {
				select {
				case ev, open := <-ch:
					if !open {
						goto finished
					}
					var step operations.Step
					if err := json.Unmarshal(ev.Data, &step); err == nil &&
						(!snapshotMode || step.Seq > rec.SeqLast) {
						writeSSE(flusher, w, ev.Seq, "op.step", step)
					}
				default:
					goto finished
				}
			}
		finished:
			final := op.Snapshot()
			writeSSE(flusher, w, final.SeqLast, "op.finished", finishedPayload(final))
			return
		}
	}
}

// finishedPayload is the op.finished event body.
type finishedBody struct {
	Status operations.Status `json:"status"`
	Error  string            `json:"error,omitempty"`
}

func finishedPayload(rec operations.Record) finishedBody {
	return finishedBody{Status: rec.Status, Error: rec.Error}
}

// writeSSE emits one SSE event with a resumable id line.
func writeSSE(flusher http.Flusher, w http.ResponseWriter, id int64, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, data)
	flusher.Flush()
}

// parseResumePosition reads the resume point from ?since= or Last-Event-ID.
// Since 0 means "no position". Writes 400 and returns ok=false on garbage.
func parseResumePosition(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("since"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		writeError(r.Context(), w, http.StatusBadRequest,
			errors.New("invalid resume position"), nil)
		return 0, false
	}
	return n, true
}

// writeOperationLookupError maps registry lookup errors to HTTP responses.
// Per the operations contract, malformed and unknown IDs are both 404 on a
// lookup (a malformed *request header* on POST is the 400 case, handled in
// beginOperation); only expired IDs that once existed answer 410.
func (h *Handler) writeOperationLookupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, operations.ErrExpired):
		writeOperationExpired(w, r.PathValue("id"))
	default:
		writeError(r.Context(), w, http.StatusNotFound, err, nil)
	}
}
