package operations

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dumpstore/internal/ansible"
)

func testConfig() Config {
	return Config{
		MaxStepsPerOperation: 8,
		MaxStepMsgBytes:      32,
		RetainMax:            5,
		RetentionTTL:         time.Hour,
		TombstoneMax:         3,
	}
}

// fakeClock returns a controllable time source.
type fakeClock struct{ t time.Time }

func newTestManager(cfg Config) (*Manager, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return newManager(cfg, func() time.Time { return clk.t }), clk
}

func validClientID(n int) string {
	return fmt.Sprintf("op_00000000-0000-0000-0000-%012x", n)
}

func step(name, status string) ansible.TaskStep {
	return ansible.TaskStep{Name: name, Status: status, Msg: "ok"}
}

func TestStartAssignsIDsAndRejectsBadID(t *testing.T) {
	m, _ := newTestManager(testConfig())

	op, outcome, err := m.Start("", "dataset.create", "tank/x", "req-1")
	if err != nil || outcome != StartCreated {
		t.Fatalf("server id start: op=%v outcome=%v err=%v", op, outcome, err)
	}
	if len(op.ID()) != 24 {
		t.Fatalf("server id = %q, want 24 hex chars", op.ID())
	}

	cid := validClientID(1)
	op2, outcome, err := m.Start(cid, "dataset.create", "tank/y", "req-2")
	if err != nil || outcome != StartCreated || op2.ID() != cid {
		t.Fatalf("client id start: %v %v %v", op2, outcome, err)
	}

	if _, _, err := m.Start("op_guess", "a", "b", ""); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad id err = %v, want ErrInvalidID", err)
	}
}

func TestStartIdempotentDoesNotReexecute(t *testing.T) {
	m, clk := newTestManager(testConfig())
	cid := validClientID(7)

	op1, o1, err := m.Start(cid, "user.create", "alice", "r1")
	if err != nil || o1 != StartCreated {
		t.Fatalf("first start: %v %v", o1, err)
	}
	executions := 0
	m.AddStep(op1, step("create user", "changed"))
	executions++

	// Replayed while still running.
	op2, o2, err := m.Start(cid, "user.create", "alice", "r2")
	if err != nil || o2 != StartReplayRunning || op1 != op2 {
		t.Fatalf("running replay: %p %v %v", op2, o2, err)
	}

	m.Complete(op1, 200)
	clk.t = clk.t.Add(time.Minute)

	// Replayed after terminal: same operation, still one execution.
	op3, o3, err := m.Start(cid, "user.create", "alice", "r3")
	if err != nil || o3 != StartReplayTerminal || op1 != op3 {
		t.Fatalf("terminal replay: %v %v", o3, err)
	}
	if executions != 1 {
		t.Fatalf("operation executed %d times, want 1", executions)
	}
	rec := op3.Snapshot()
	if rec.Status != StatusSucceeded || rec.ID != cid {
		t.Fatalf("replayed record: %+v", rec)
	}
}

func TestAddStepGaplessSeqAndRing(t *testing.T) {
	m, _ := newTestManager(testConfig())
	op, _, _ := m.Start(validClientID(2), "x", "", "")
	for i := 0; i < 20; i++ {
		m.AddStep(op, step(fmt.Sprintf("task %d", i), "changed"))
	}
	rec := op.Snapshot()
	if rec.SeqLast != 20 {
		t.Fatalf("SeqLast = %d, want 20", rec.SeqLast)
	}
	if len(rec.Steps) != 8 {
		t.Fatalf("ring retained %d, want 8", len(rec.Steps))
	}
	if rec.SeqFirst != 13 || rec.Steps[0].Seq != 13 || rec.Steps[7].Seq != 20 {
		t.Fatalf("ring window: first=%d steps=%v", rec.SeqFirst, rec.Steps)
	}
	// Every step seq gapless in its time: assert published journal saw 1..20
	events, latest := op.Journal().Replay(0)
	if latest != 20 || len(events) != 8 {
		t.Fatalf("journal latest=%d retained=%d", latest, len(events))
	}
}

func TestAddStepTruncatesMessage(t *testing.T) {
	m, _ := newTestManager(testConfig())
	op, _, _ := m.Start(validClientID(3), "x", "", "")
	m.AddStep(op, ansible.TaskStep{Name: "big", Status: "changed", Msg: strings.Repeat("x", 200)})
	rec := op.Snapshot()
	if len(rec.Steps[0].Msg) > 32 || !strings.HasSuffix(rec.Steps[0].Msg, "…") {
		t.Fatalf("truncation: %q (%d bytes)", rec.Steps[0].Msg, len(rec.Steps[0].Msg))
	}
}

func TestFinishStatesAndPrecedence(t *testing.T) {
	m, _ := newTestManager(testConfig())

	// Failed then timeout → timeout wins.
	op, _, _ := m.Start(validClientID(4), "a", "", "")
	op.MarkFailure("boom")
	op.MarkTimeout("deadline")
	m.Complete(op, 500)
	if got := op.Snapshot().Status; got != StatusTimeout {
		t.Fatalf("status = %s, want timeout", got)
	}
	if op.Snapshot().Error != "deadline" {
		t.Fatalf("error = %q", op.Snapshot().Error)
	}

	// Timeout then failure → timeout retained.
	op2, _, _ := m.Start(validClientID(5), "b", "", "")
	op2.MarkTimeout("late")
	op2.MarkFailure("boom")
	m.Complete(op2, 500)
	if got := op2.Snapshot().Status; got != StatusTimeout {
		t.Fatalf("status = %s, want timeout", got)
	}

	// Plain failure.
	op3, _, _ := m.Start(validClientID(6), "c", "", "")
	op3.MarkFailure("nope")
	m.Complete(op3, 500)
	if got := op3.Snapshot().Status; got != StatusFailed {
		t.Fatalf("status = %s, want failed", got)
	}

	// Success inferred from 2xx.
	op4, _, _ := m.Start(validClientID(8), "d", "", "")
	m.Complete(op4, 200)
	if got := op4.Snapshot().Status; got != StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", got)
	}

	// Finish is idempotent.
	m.Complete(op4, 500)
	if got := op4.Snapshot().Status; got != StatusSucceeded {
		t.Fatalf("second Complete changed status to %s", got)
	}
	if op4.Snapshot().FinishedAt.IsZero() {
		t.Fatal("FinishedAt not set")
	}
	select {
	case <-op4.Done():
	default:
		t.Fatal("Done channel not closed after finish")
	}
}

func TestGetNotFoundExpiredAndTombstone(t *testing.T) {
	cfg := testConfig()
	cfg.RetainMax = 1
	cfg.TombstoneMax = 2
	m, clk := newTestManager(cfg)

	// Unknown valid ID → not found; malformed lookup → invalid.
	if _, err := m.Get(validClientID(99)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := m.Get("not-an-id"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad lookup: %v", err)
	}

	// Finish two terminals; RetainMax=1 evicts the oldest to tombstone.
	op1, _, _ := m.Start(validClientID(10), "a", "", "")
	m.Complete(op1, 200)
	clk.t = clk.t.Add(time.Minute)
	op2, _, _ := m.Start(validClientID(11), "b", "", "")
	m.Complete(op2, 200)

	if _, err := m.Get(validClientID(10)); !errors.Is(err, ErrExpired) {
		t.Fatalf("pruned op: %v", err)
	}
	if _, _, err := m.Start(validClientID(10), "a", "", ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("replay pruned id: %v", err)
	}
	if op, err := m.Get(validClientID(11)); err != nil || op == nil {
		t.Fatalf("retained op: %v", err)
	}

	// After TTL the tombstone itself expires → not found.
	clk.t = clk.t.Add(2 * time.Hour)
	if _, err := m.Get(validClientID(10)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired tombstone: %v", err)
	}
}

func TestBoundedRetentionUnderLoad(t *testing.T) {
	cfg := testConfig()
	cfg.RetainMax = 200
	cfg.TombstoneMax = 256
	m, clk := newTestManager(cfg)

	for i := 0; i < 10_000; i++ {
		op, _, err := m.Start("", "x", "", "")
		if err != nil {
			t.Fatal(err)
		}
		m.Complete(op, 200)
		if i%97 == 0 {
			clk.t = clk.t.Add(time.Second)
		}
	}

	m.mu.Lock()
	live := len(m.ops)
	tomb := len(m.tombstones)
	running := m.runningCountLocked()
	m.mu.Unlock()

	if running != 0 {
		t.Fatalf("running = %d, want 0", running)
	}
	if live > cfg.RetainMax {
		t.Fatalf("live terminals = %d, cap %d", live, cfg.RetainMax)
	}
	if tomb > cfg.TombstoneMax {
		t.Fatalf("tombstones = %d, cap %d", tomb, cfg.TombstoneMax)
	}
	if m.AcceptedCount() != 10_000 {
		t.Fatalf("accepted = %d", m.AcceptedCount())
	}
}

func TestSubscribeReplayLiveAndTerminal(t *testing.T) {
	m, _ := newTestManager(testConfig())
	op, _, _ := m.Start(validClientID(12), "x", "", "")
	m.AddStep(op, step("a", "changed"))
	m.AddStep(op, step("b", "changed"))

	initial, ch, latest := op.Journal().Subscribe(1)
	if latest != 2 || len(initial) != 1 || initial[0].Seq != 2 {
		t.Fatalf("replay: latest=%d initial=%v", latest, initial)
	}
	m.AddStep(op, step("c", "changed"))
	select {
	case ev := <-ch:
		if ev.Seq != 3 {
			t.Fatalf("live seq = %d", ev.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("live event not delivered")
	}

	m.Complete(op, 200)
	select {
	case <-op.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
	if !op.Journal().Closed() {
		t.Fatal("journal not sealed after finish")
	}
	// Late subscription replays terminal window.
	late, _, latest := op.Journal().Subscribe(0)
	if len(late) != 3 || latest != 3 {
		t.Fatalf("late replay: %v latest=%d", late, latest)
	}
	op.Journal().Unsubscribe(ch)
}

func TestConcurrentStartSameID(t *testing.T) {
	m, _ := newTestManager(testConfig())
	cid := validClientID(42)
	const n = 50
	var wg sync.WaitGroup
	created := 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, outcome, err := m.Start(cid, "x", "", "")
			if err != nil {
				t.Errorf("start: %v", err)
				return
			}
			if outcome == StartCreated {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created %d operations for one ID, want 1", created)
	}
}
