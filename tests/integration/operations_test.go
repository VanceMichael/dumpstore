//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// opRecord is the GET /api/operations/{id} response (subset asserted on).
type opRecord struct {
	ID         string `json:"operation_id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	SeqFirst   int64  `json:"seq_first"`
	SeqLast    int64  `json:"seq_last"`
	Error      string `json:"error"`
	Steps      []struct {
		Seq    int64  `json:"seq"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"steps"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

type opReplay struct {
	ID     string `json:"operation_id"`
	Status string `json:"status"`
}

func opUUID(t *testing.T) string {
	t.Helper()
	return "op_" + strings.TrimSpace(vmExec(t, "cat /proc/sys/kernel/random/uuid"))
}

// postWithOp issues a change request carrying an operation ID, returning
// status, body and the echoed operation header.
func postWithOp(t *testing.T, method, path, opID string, body any) (int, []byte, string) {
	t.Helper()
	login(t)
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if opID != "" {
		req.Header.Set("X-Operation-ID", opID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header.Get("X-Operation-ID")
}

func getOperation(t *testing.T, opID string) (int, opRecord) {
	t.Helper()
	status, b := api(t, "GET", "/api/operations/"+opID, nil)
	var rec opRecord
	if status == 200 {
		rec = decode[opRecord](t, b)
	}
	return status, rec
}

// streamEvents opens the per-operation SSE stream with an optional resume
// position and reads until the terminal record/finished event (or times out).
func streamEvents(t *testing.T, opID, lastEventID string, timeout time.Duration) []string {
	t.Helper()
	login(t)
	req, err := http.NewRequest("GET", baseURL+"/api/operations/"+opID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events status = %d", resp.StatusCode)
	}
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "id: ") ||
			strings.HasPrefix(line, "data: ") {
			lines = append(lines, line)
		}
		// Terminal snapshot/finished data line ends the collection.
		if strings.HasPrefix(line, "data: ") &&
			(strings.Contains(line, `"status":"succeeded"`) ||
				strings.Contains(line, `"status":"failed"`) ||
				strings.Contains(line, `"status":"timeout"`)) {
			break
		}
	}
	return lines
}

// waitForTerminal polls the operation until it leaves running.
func waitForTerminal(t *testing.T, opID string) opRecord {
	t.Helper()
	var rec opRecord
	waitFor(t, "operation "+opID+" terminal", 30*time.Second, func() bool {
		status, r := getOperation(t, opID)
		if status != 200 {
			return false
		}
		rec = r
		return rec.Status != "running"
	})
	return rec
}

func TestOperationHeaderAndRecord(t *testing.T) {
	opID := opUUID(t)
	status, body, echo := postWithOp(t, "POST", "/api/auto-snapshot/takeover", opID, nil)
	if status != 200 {
		t.Fatalf("takeover status = %d: %s", status, truncate(body, 500))
	}
	if echo != opID {
		t.Fatalf("X-Operation-ID echo = %q, want %q", echo, opID)
	}
	recStatus, rec := getOperation(t, opID)
	if recStatus != 200 {
		t.Fatalf("GET operation status = %d", recStatus)
	}
	if rec.ID != opID || rec.Action != "auto_snapshot.takeover" || rec.Status != "succeeded" {
		t.Fatalf("record: %+v", rec)
	}
	if rec.SeqLast < 1 || len(rec.Steps) < 1 || rec.FinishedAt == "" {
		t.Fatalf("record missing steps/timestamps: %+v", rec)
	}
	for i, s := range rec.Steps {
		if s.Seq != rec.SeqFirst+int64(i) {
			t.Fatalf("step %d seq = %d (seq_first=%d)", i, s.Seq, rec.SeqFirst)
		}
	}
}

func TestOperationFailureTerminal(t *testing.T) {
	// A dataset under a non-existent pool passes Go validation but fails in
	// the zfs_dataset_create playbook.
	opID := opUUID(t)
	status, body, echo := postWithOp(t, "POST", "/api/datasets", opID, map[string]string{
		"name": "definitely-no-such-pool-xyz/ops-test",
		"type": "filesystem",
	})
	if status != 500 {
		t.Fatalf("expected 500, got %d: %s", status, truncate(body, 500))
	}
	if echo != opID {
		t.Fatalf("failure response missing operation header")
	}
	rec := waitForTerminal(t, opID)
	if rec.Status != "failed" || rec.Error == "" {
		t.Fatalf("expected failed with error, got %+v", rec)
	}
}

func TestOperationValidationRejectHasNoOperation(t *testing.T) {
	// Malformed operation ID → 400, nothing accepted.
	status, _, echo := postWithOp(t, "POST", "/api/datasets", "nonsense-id", map[string]string{
		"name": "tank/x", "type": "filesystem",
	})
	if status != 400 {
		t.Fatalf("malformed id status = %d", status)
	}
	if echo != "" {
		t.Fatalf("malformed id must not echo an operation header")
	}

	// Valid ID but request fails validation → no operation is created.
	opID := opUUID(t)
	status, _, echo = postWithOp(t, "POST", "/api/datasets", opID, map[string]string{
		"name": "bad name!!", "type": "filesystem",
	})
	if status != 400 {
		t.Fatalf("validation status = %d", status)
	}
	if echo != "" {
		t.Fatalf("rejected request must not echo an operation header")
	}
	if code, _ := getOperation(t, opID); code != 404 {
		t.Fatalf("rejected request created an operation: GET status = %d", code)
	}
}

func TestOperationReplayIsIdempotent(t *testing.T) {
	opID := opUUID(t)
	status, body1, _ := postWithOp(t, "POST", "/api/auto-snapshot/takeover", opID, nil)
	if status != 200 {
		t.Fatalf("initial: %d %s", status, truncate(body1, 300))
	}
	_, first := getOperation(t, opID)

	status2, body2, _ := postWithOp(t, "POST", "/api/auto-snapshot/takeover", opID, nil)
	if status2 != 200 {
		t.Fatalf("replay status = %d", status2)
	}
	re := decode[opReplay](t, body2)
	if re.ID != opID || re.Status != "succeeded" {
		t.Fatalf("replay body: %s", truncate(body2, 300))
	}
	_, second := getOperation(t, opID)
	// The frozen record is untouched by the replay (no second execution).
	if second.StartedAt != first.StartedAt || second.FinishedAt != first.FinishedAt ||
		second.SeqLast != first.SeqLast {
		t.Fatalf("record changed on replay:\n%+v\n%+v", first, second)
	}
}

func TestOperationExpired410(t *testing.T) {
	// Default retention keeps 200 terminal operations. Create one via the
	// Ansible lane, then flood cheap ops-lane terminal operations (repeated
	// scrub starts against the test pool return in milliseconds — whether
	// they succeed or fail, each is a terminal operation).
	opID := opUUID(t)
	if status, _, _ := postWithOp(t, "POST", "/api/auto-snapshot/takeover", opID, nil); status != 200 {
		t.Fatalf("seed status = %d", status)
	}
	for i := 0; i < 201; i++ {
		code, _ := api(t, "POST", "/api/scrub/"+testPool, nil)
		if code != 200 && code != 500 {
			t.Fatalf("flood %d unexpected status = %d", i, code)
		}
	}
	if code, body := api(t, "GET", "/api/operations/"+opID, nil); code != 410 ||
		!strings.Contains(string(body), "recoverable") {
		t.Fatalf("expired GET = %d %s", code, truncate(body, 200))
	}
	// Re-POST of an expired ID is also a definitive 410.
	if code, body, _ := postWithOp(t, "POST", "/api/auto-snapshot/takeover", opID, nil); code != 410 {
		t.Fatalf("expired replay = %d %s", code, truncate(body, 200))
	}
}

func TestOperationStreamSnapshotAndResume(t *testing.T) {
	opID := opUUID(t)
	if status, _, _ := postWithOp(t, "POST", "/api/auto-snapshot/takeover", opID, nil); status != 200 {
		t.Fatalf("seed status = %d", status)
	}
	_, rec := getOperation(t, opID)

	// Fresh connect: snapshot full replace + id lines.
	lastSeq := strconv.FormatInt(rec.SeqLast, 10)
	lines := streamEvents(t, opID, "", 10*time.Second)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "event: op.snapshot") ||
		!strings.Contains(joined, "id: "+lastSeq) ||
		!strings.Contains(joined, opID) {
		t.Fatalf("snapshot stream missing pieces:\n%s", joined)
	}

	// Resume at the last confirmed seq: snapshot must not repeat; the
	// terminal finished event alone is delivered.
	lines2 := streamEvents(t, opID, lastSeq, 10*time.Second)
	joined2 := strings.Join(lines2, "\n")
	if strings.Contains(joined2, "op.snapshot") {
		t.Fatalf("resume at latest seq re-sent snapshot:\n%s", joined2)
	}
	if !strings.Contains(joined2, "event: op.finished") {
		t.Fatalf("resume missing finished:\n%s", joined2)
	}
}

func TestOperationStreamsAreIsolated(t *testing.T) {
	idA := opUUID(t)
	idB := opUUID(t)
	if s, _, _ := postWithOp(t, "POST", "/api/auto-snapshot/takeover", idA, nil); s != 200 {
		t.Fatal(s)
	}
	if s, _, _ := postWithOp(t, "POST", "/api/auto-snapshot/release", idB, nil); s != 200 {
		t.Fatal(s)
	}
	a := streamEvents(t, idA, "", 5*time.Second)
	b := streamEvents(t, idB, "", 5*time.Second)
	ja := strings.Join(a, "\n")
	jb := strings.Join(b, "\n")
	if !strings.Contains(ja, idA) || strings.Contains(ja, idB) {
		t.Fatalf("stream A leaked another operation:\n%s", ja)
	}
	if !strings.Contains(jb, idB) || strings.Contains(jb, idA) {
		t.Fatalf("stream B leaked another operation:\n%s", jb)
	}
	if !strings.Contains(jb, "auto_snapshot.release") ||
		!strings.Contains(ja, "auto_snapshot.takeover") {
		t.Fatalf("streams returned wrong records:\nA:%s\nB:%s", ja, jb)
	}
}

func TestOperationSurvivesClientDisconnect(t *testing.T) {
	opID := opUUID(t)
	login(t)

	// Fire the request through a context we cancel shortly after dispatch;
	// the runners detach client cancellation, so the change must run to a
	// queryable terminal state anyway.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST",
		baseURL+"/api/auto-snapshot/takeover", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Operation-ID", opID)
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close() // response may have arrived or been interrupted
	}

	rec := waitForTerminal(t, opID)
	if rec.Status != "succeeded" {
		t.Fatalf("background completion status = %s (%s)", rec.Status, rec.Error)
	}
}

func TestJobsLaneUnaffected(t *testing.T) {
	// The jobs channel keeps answering; operation tracking does not wrap it.
	status, b := api(t, "GET", "/api/jobs", nil)
	if status != 200 {
		t.Fatalf("jobs status = %d: %s", status, b)
	}
	// Cancelling a nonexistent job stays in the jobs lane (4xx, no contract
	// on operation headers).
	status, _ = api(t, "POST", "/api/jobs/does-not-exist/cancel", nil)
	if status != 400 && status != 404 {
		t.Fatalf("unexpected jobs cancel status = %d", status)
	}
}
