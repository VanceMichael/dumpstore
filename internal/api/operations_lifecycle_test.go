package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dumpstore/internal/ansible"
	"dumpstore/internal/broker"
	"dumpstore/internal/operations"
	"dumpstore/internal/ops"
)

// fakeExec controls the behaviour of the playbook execution seam in tests.
type fakeExec struct {
	mu      sync.Mutex
	execs   int
	gate    chan struct{} // non-nil: block until closed
	fail    bool
	timeout bool
	steps   []ansible.TaskStep
}

func (f *fakeExec) runPlaybook(ctx context.Context, playbook string, vars map[string]string, onStep func(ansible.TaskStep)) (*ansible.PlaybookOutput, error) {
	f.mu.Lock()
	f.execs++
	gate := f.gate
	fail, timeout := f.fail, f.timeout
	steps := append([]ansible.TaskStep(nil), f.steps...)
	f.mu.Unlock()

	if gate != nil {
		<-gate
	}
	for _, s := range steps {
		onStep(s)
	}
	if timeout {
		return ansible.NewPlaybookOutput(steps), fmt.Errorf("%w: playbook %s after 5m0s", ansible.ErrTimedOut, playbook)
	}
	if fail {
		return ansible.NewPlaybookOutput(steps), fmt.Errorf("task %q failed: boom", "fail task")
	}
	return ansible.NewPlaybookOutput(steps), nil
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs
}

// newOpTestHandler wires a Handler with the operation manager and registers
// one synthetic mutation route that exercises beginOperation → runOp →
// writeJSON/writeRunOpError exactly like a real handler.
func newOpTestHandler(t *testing.T, cfg operations.Config, fx *fakeExec) (*Handler, http.Handler) {
	t.Helper()
	h := &Handler{
		broker: broker.New(),
		opMgr:  operations.NewManager(cfg),
	}
	h.execPlaybook = fx.runPlaybook

	mux := http.NewServeMux()
	mux.HandleFunc("POST /test-op", func(w http.ResponseWriter, r *http.Request) {
		// Emulate the request-ID middleware for audit correlation.
		r = r.WithContext(WithReqID(r.Context(), "req-test-1"))
		w, r, replayed := h.beginOperation(w, r, "test.op", "target-x")
		if replayed {
			return
		}
		out, err := h.runOp(r.Context(), "fake.yml", nil)
		auditLog(r.Context(), r, "test.op", "target-x", err)
		if err != nil {
			writeRunOpError(r.Context(), w, err, out)
			return
		}
		writeJSON(r.Context(), w, map[string]any{"tasks": out.Steps()})
	})
	h.RegisterRoutes(mux)
	return h, mux
}

func defaultFake() *fakeExec {
	return &fakeExec{steps: []ansible.TaskStep{
		{Name: "assert", Status: "ok"},
		{Name: "apply", Status: "changed", Msg: "done"},
	}}
}

func doPost(t *testing.T, hnd http.Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/test-op", nil)
	if id != "" {
		req.Header.Set(OperationIDHeader, id)
	}
	rec := httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	return rec
}

func TestBeginOperationSuccessLifecycle(t *testing.T) {
	fx := defaultFake()
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_11111111-1111-1111-1111-111111111111"

	rec := doPost(t, hnd, id)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(OperationIDHeader) != id {
		t.Fatalf("missing operation header: %q", rec.Header().Get(OperationIDHeader))
	}
	var body struct {
		Tasks []ansible.TaskStep `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 2 {
		t.Fatalf("tasks = %d", len(body.Tasks))
	}

	op, err := h.opMgr.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	got := op.Snapshot()
	if got.Status != operations.StatusSucceeded || got.SeqLast != 2 || got.Action != "test.op" || got.ReqID != "req-test-1" {
		t.Fatalf("record: %+v", got)
	}
	if got.Steps[0].Seq != 1 || got.Steps[1].Seq != 2 {
		t.Fatalf("seq ordering: %+v", got.Steps)
	}
}

func TestBeginOperationFailureReplayNoReexecute(t *testing.T) {
	fx := defaultFake()
	fx.fail = true
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_22222222-2222-2222-2222-222222222222"

	rec := doPost(t, hnd, id)
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get(OperationIDHeader) != id {
		t.Fatal("operation header missing on failure")
	}

	op, _ := h.opMgr.Get(id)
	if op.Snapshot().Status != operations.StatusFailed || op.Snapshot().Error == "" {
		t.Fatalf("record: %+v", op.Snapshot())
	}

	// Re-POST the same ID: deterministic terminal replay, no second run.
	rec2 := doPost(t, hnd, id)
	if rec2.Code != 500 || !strings.Contains(rec2.Body.String(), "operation_id") {
		t.Fatalf("replay: %d %s", rec2.Code, rec2.Body.String())
	}
	if fx.count() != 1 {
		t.Fatalf("executed %d times, want 1", fx.count())
	}
}

func TestBeginOperationTimeoutClassified(t *testing.T) {
	fx := defaultFake()
	fx.timeout = true
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_33333333-3333-3333-3333-333333333333"

	if rec := doPost(t, hnd, id); rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	op, _ := h.opMgr.Get(id)
	if op.Snapshot().Status != operations.StatusTimeout {
		t.Fatalf("status = %s, want timeout", op.Snapshot().Status)
	}
}

func TestBeginOperationReplayRunning409(t *testing.T) {
	fx := defaultFake()
	fx.gate = make(chan struct{})
	_, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_44444444-4444-4444-4444-444444444444"

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- doPost(t, hnd, id) }()

	// Wait until the first execution is inside the gate.
	deadline := time.After(time.Second)
	for fx.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("first execution never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	rec := doPost(t, hnd, id)
	if rec.Code != http.StatusConflict {
		t.Fatalf("running replay status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"running"`) {
		t.Fatalf("409 body: %s", rec.Body.String())
	}

	close(fx.gate)
	first := <-firstDone
	if first.Code != 200 {
		t.Fatalf("first request: %d", first.Code)
	}
	if fx.count() != 1 {
		t.Fatalf("executed %d times, want 1", fx.count())
	}
}

func TestBeginOperationInvalidID(t *testing.T) {
	fx := defaultFake()
	_, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	rec := doPost(t, hnd, "not-a-uuid")
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if fx.count() != 0 {
		t.Fatal("no execution must happen for a malformed operation ID")
	}
}

func TestBeginOperationExpired410(t *testing.T) {
	cfg := operations.Config{RetainMax: 1, RetentionTTL: time.Hour, TombstoneMax: 4}
	fx := defaultFake()
	h, hnd := newOpTestHandler(t, cfg, fx)

	id1 := "op_55555555-5555-5555-5555-555555555555"
	if rec := doPost(t, hnd, id1); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	id2 := "op_66666666-6666-6666-6666-666666666666"
	if rec := doPost(t, hnd, id2); rec.Code != 200 {
		t.Fatalf("second: %d", rec.Code)
	}

	// id1 is now tombstoned (RetainMax=1).
	if _, err := h.opMgr.Get(id1); err != operations.ErrExpired {
		t.Fatalf("Get: %v", err)
	}
	rec := doPost(t, hnd, id1)
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), `"recoverable":false`) {
		t.Fatalf("expired replay: %d %s", rec.Code, rec.Body.String())
	}

	// Direct GET of an expired ID is 410 as well.
	req := httptest.NewRequest("GET", "/api/operations/"+id1, nil)
	g := httptest.NewRecorder()
	hnd.ServeHTTP(g, req)
	if g.Code != http.StatusGone {
		t.Fatalf("GET expired: %d", g.Code)
	}
	// Unknown valid ID → 404.
	req = httptest.NewRequest("GET", "/api/operations/op_77777777-7777-7777-7777-777777777777", nil)
	g = httptest.NewRecorder()
	hnd.ServeHTTP(g, req)
	if g.Code != http.StatusNotFound {
		t.Fatalf("GET unknown: %d", g.Code)
	}
}

// failingWriter simulates a client that has gone away: every write fails.
// The mutation must still reach a queryable terminal state.
type failingWriter struct {
	header http.Header
}

func (f *failingWriter) Header() http.Header { return f.header }
func (f *failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}
func (f *failingWriter) WriteHeader(int) {}

func TestBeginOperationDisconnectedWriterStillFinalizes(t *testing.T) {
	fx := defaultFake()
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_88888888-8888-8888-8888-888888888888"

	req := httptest.NewRequest("POST", "/test-op", nil)
	req.Header.Set(OperationIDHeader, id)
	hnd.ServeHTTP(&failingWriter{header: http.Header{}}, req)

	op, err := h.opMgr.Get(id)
	if err != nil {
		t.Fatalf("query after disconnect: %v", err)
	}
	if op.Snapshot().Status != operations.StatusSucceeded {
		t.Fatalf("status = %s", op.Snapshot().Status)
	}
}

// TestOperationEventsTerminalSnapshotAndResume verifies the SSE protocol
// for an already terminal operation.
func TestOperationEventsTerminal(t *testing.T) {
	fx := defaultFake()
	_, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_99999999-9999-9999-9999-999999999999"
	if rec := doPost(t, hnd, id); rec.Code != 200 {
		t.Fatal(rec.Code)
	}

	// Fresh subscribe → snapshot, then the handler returns (terminal).
	req := httptest.NewRequest("GET", "/api/operations/"+id+"/events", nil)
	rec := httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "id: 2") || !strings.Contains(body, "event: op.snapshot") ||
		!strings.Contains(body, `"operation_id"`) {
		t.Fatalf("snapshot stream: %q", body)
	}

	// since=1 → incremental: step seq 2 then finished.
	req = httptest.NewRequest("GET", "/api/operations/"+id+"/events?since=1", nil)
	rec = httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, "id: 2\nevent: op.step") || !strings.Contains(body, "event: op.finished") {
		t.Fatalf("incremental stream: %q", body)
	}
	if strings.Contains(body, "op.snapshot") {
		t.Fatalf("incremental resume must not resend snapshot: %q", body)
	}

	// Garbage since → 400.
	req = httptest.NewRequest("GET", "/api/operations/"+id+"/events?since=abc", nil)
	rec = httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad since: %d", rec.Code)
	}
}

// TestOperationEventsLiveTail verifies snapshot → live op.step → finished
// for an operation that is still running when the stream connects.
func TestOperationEventsLiveTail(t *testing.T) {
	fx := defaultFake()
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	srv := httptest.NewServer(hnd)
	defer srv.Close()

	id := "op_abababab-abab-abab-abab-abababababab"
	op, outcome, err := h.opMgr.Start(id, "test.op", "live", "req-live")
	if err != nil || outcome != operations.StartCreated {
		t.Fatalf("start: %v %v", err, outcome)
	}

	resp, err := http.Get(srv.URL + "/api/operations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	readLine := func() string {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return strings.TrimSpace(line)
	}
	readEvent := func() (idLine, event, data string) {
		for {
			line := readLine()
			if strings.HasPrefix(line, "id: ") {
				idLine = line
			} else if strings.HasPrefix(line, "event: ") {
				event = line
			} else if strings.HasPrefix(line, "data: ") {
				data = line
				return idLine, event, data
			}
		}
	}

	idLine, event, _ := readEvent()
	if event != "event: op.snapshot" || idLine != "id: 0" {
		t.Fatalf("initial: %q %q", event, idLine)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		h.opMgr.AddStep(op, ansible.TaskStep{Name: "live-task", Status: "changed"})
		time.Sleep(20 * time.Millisecond)
		h.opMgr.Complete(op, 200)
	}()

	idLine, event, data := readEvent()
	if event != "event: op.step" || idLine != "id: 1" || !strings.Contains(data, "live-task") {
		t.Fatalf("step event: %q %q %q", idLine, event, data)
	}
	idLine, event, data = readEvent()
	if event != "event: op.finished" || !strings.Contains(data, "succeeded") {
		t.Fatalf("finished event: %q %q %q", idLine, event, data)
	}
}

// TestOperationEventsJournalByteWindowTriggersSnapshot covers REV-1: the
// record ring retains up to 500 steps but the replay journal byte-caps at
// 512 KiB. A resume position older than the journal window (yet still inside
// the record ring) must converge with a full snapshot, not an incremental
// replay with a silent gap.
func TestOperationEventsJournalByteWindowTriggersSnapshot(t *testing.T) {
	fx := defaultFake()
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	h.execPlaybook = func(ctx context.Context, playbook string, vars map[string]string, onStep func(ansible.TaskStep)) (*ansible.PlaybookOutput, error) {
		big := strings.Repeat("x", 16*1024)
		var steps []ansible.TaskStep
		for i := 0; i < 40; i++ {
			s := ansible.TaskStep{Name: fmt.Sprintf("t%d", i), Status: "changed", Msg: big}
			steps = append(steps, s)
			onStep(s)
		}
		return ansible.NewPlaybookOutput(steps), nil
	}
	id := "op_dededede-dede-dede-dede-dededededede"
	if rec := doPost(t, hnd, id); rec.Code != 200 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	op, err := h.opMgr.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	journalOldest := op.Journal().OldestSeq()
	recordFirst := op.Snapshot().SeqFirst
	if journalOldest <= recordFirst {
		t.Fatalf("test setup expects journal window narrower than record ring: journal=%d record=%d", journalOldest, recordFirst)
	}

	// since=1 sits inside the record ring but before the journal window —
	// a snapshot (not a gap-ridden incremental replay) is mandatory.
	req := httptest.NewRequest("GET", "/api/operations/"+id+"/events?since=1", nil)
	rec := httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: op.snapshot") {
		t.Fatalf("pre-journal-window resume must send snapshot:\n%.200s", body)
	}

	// A position inside the journal window stays incremental.
	req = httptest.NewRequest("GET", "/api/operations/"+id+"/events?since="+itoa(journalOldest), nil)
	rec = httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "op.snapshot") {
		t.Fatalf("inside-window resume must stay incremental")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestOperationLookupMalformedIDIs404(t *testing.T) {
	fx := defaultFake()
	_, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	for _, path := range []string{"/api/operations/not-an-id", "/api/operations/not-an-id/events"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		hnd.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s -> %d, want 404", path, rec.Code)
		}
	}
}

func TestOperationMetricsEmitted(t *testing.T) {
	fx := defaultFake()
	h, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	h.runner = ansible.NewRunner("")
	h.ops = ops.NewRunner()

	id := "op_cafebabc-cafe-cafe-cafe-cafebabecafe"
	if rec := doPost(t, hnd, id); rec.Code != 200 {
		t.Fatal(rec.Code)
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	hnd.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		"dumpstore_operations_accepted_total 1",
		`dumpstore_operations_finished_total{status="succeeded"} 1`,
		"dumpstore_operations_running 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}

func TestAuditLineCarriesOpAndReqID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fx := defaultFake()
	_, hnd := newOpTestHandler(t, operations.DefaultConfig(), fx)
	id := "op_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if rec := doPost(t, hnd, id); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "op_id="+id) || !strings.Contains(out, "req_id=req-test-1") {
		t.Fatalf("audit correlation missing:\n%s", out)
	}
}
