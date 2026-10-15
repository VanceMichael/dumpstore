package broker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func drain(ch chan Event, timeout time.Duration) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(timeout):
			return out
		}
	}
}

func TestJournalPublishAssignsMonotonicSeq(t *testing.T) {
	j := NewJournal(100, 1<<20)
	for i := int64(1); i <= 10; i++ {
		ev, err := j.Publish(i)
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if ev.Seq != i {
			t.Fatalf("seq = %d, want %d", ev.Seq, i)
		}
	}
	events, latest := j.Replay(0)
	if latest != 10 || len(events) != 10 {
		t.Fatalf("replay: latest=%d len=%d", latest, len(events))
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("replay seq at %d = %d", i, ev.Seq)
		}
	}
}

func TestJournalReplayAfter(t *testing.T) {
	j := NewJournal(100, 1<<20)
	for i := 0; i < 5; i++ {
		if _, err := j.Publish(i); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		after   int64
		wantLen int
		wantSeq int64 // first expected seq
	}{
		{0, 5, 1}, // from start
		{2, 3, 3}, // strict >
		{5, 0, 0}, // caught up: nothing new
		{99, 0, 0},
	}
	for _, tt := range tests {
		events, latest := j.Replay(tt.after)
		if len(events) != tt.wantLen {
			t.Errorf("after=%d: got %d events, want %d", tt.after, len(events), tt.wantLen)
		}
		if tt.wantLen > 0 && (events[0].Seq != tt.wantSeq || latest != 5) {
			t.Errorf("after=%d: first=%d latest=%d", tt.after, events[0].Seq, latest)
		}
	}
}

func TestJournalSubscribeReplayThenLive(t *testing.T) {
	j := NewJournal(100, 1<<20)
	for i := 0; i < 3; i++ {
		if _, err := j.Publish(i); err != nil {
			t.Fatal(err)
		}
	}
	initial, ch, latest := j.Subscribe(2)
	if latest != 3 || len(initial) != 1 || initial[0].Seq != 3 {
		t.Fatalf("initial replay: latest=%d initial=%v", latest, initial)
	}
	for i := 3; i < 6; i++ {
		if _, err := j.Publish(i); err != nil {
			t.Fatal(err)
		}
	}
	live := drain(ch, 200*time.Millisecond)
	if len(live) != 3 {
		t.Fatalf("got %d live events, want 3", len(live))
	}
	for i, ev := range live {
		if ev.Seq != int64(i+4) {
			t.Fatalf("live[%d] seq=%d, want %d", i, ev.Seq, i+4)
		}
	}
	j.Unsubscribe(ch)
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after Unsubscribe")
	}
	// Unsubscribe twice is a no-op.
	j.Unsubscribe(ch)
}

func TestJournalSlowSubscriberDroppedOthersUnaffected(t *testing.T) {
	j := NewJournal(1000, 1<<20)
	_, chSlow, _ := j.Subscribe(0)
	_, chFast, _ := j.Subscribe(0)

	// The fast subscriber drains continuously; the slow one never reads.
	var fastMu sync.Mutex
	var fastSeqs []int64
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		for ev := range chFast {
			fastMu.Lock()
			fastSeqs = append(fastSeqs, ev.Seq)
			fastMu.Unlock()
		}
	}()

	const published = subscriberBufferSize + 10
	publishDone := make(chan struct{})
	go func() {
		for i := 0; i < published; i++ {
			if _, err := j.Publish(i); err != nil {
				t.Errorf("publish: %v", err)
				return
			}
		}
		close(publishDone)
	}()
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("publisher blocked on slow subscriber")
	}

	// Slow channel gets closed by the journal.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-chSlow:
			if !ok {
				goto dropped
			}
		case <-deadline:
			t.Fatal("slow subscriber was not dropped")
		}
	}
dropped:

	j.Unsubscribe(chFast)
	<-fastDone
	fastMu.Lock()
	got := len(fastSeqs)
	fastMu.Unlock()
	if got != published {
		t.Fatalf("fast subscriber got %d events, want %d", got, published)
	}

	// A fresh subscription replays the whole retained sequence: the
	// slow consumer reconnects along the same path without a gap.
	initial, ch2, latest := j.Subscribe(0)
	if latest != published {
		t.Fatalf("latest = %d, want %d", latest, published)
	}
	seqs := map[int64]bool{}
	for _, ev := range initial {
		seqs[ev.Seq] = true
	}
	for _, ev := range drain(ch2, 100*time.Millisecond) {
		seqs[ev.Seq] = true
	}
	if len(seqs) != published {
		t.Fatalf("reconnect replay has %d unique seqs, want %d", len(seqs), published)
	}
	j.Unsubscribe(ch2)
}

func TestJournalStepCapEvictsOldest(t *testing.T) {
	j := NewJournal(5, 1<<20)
	for i := 0; i < 20; i++ {
		if _, err := j.Publish(i); err != nil {
			t.Fatal(err)
		}
	}
	events, latest := j.Replay(0)
	if len(events) != 5 {
		t.Fatalf("retained %d events, want 5", len(events))
	}
	if events[0].Seq != 16 || events[4].Seq != 20 || latest != 20 {
		t.Fatalf("eviction window: %+v latest=%d", events, latest)
	}
	if j.OldestSeq() != 16 {
		t.Fatalf("OldestSeq = %d, want 16", j.OldestSeq())
	}
}

func TestJournalBytesCapEvictsOldest(t *testing.T) {
	// Each payload marshals to 2 bytes ("42"); cap at ~5 payloads.
	j := NewJournal(1000, 10)
	for i := 0; i < 10; i++ {
		if _, err := j.Publish(42); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := j.Replay(0)
	if len(events) != 5 {
		t.Fatalf("bytes cap retained %d events, want 5", len(events))
	}
	// Newest oversized event is always retained.
	j2 := NewJournal(1000, 1)
	if _, err := j2.Publish("a payload much larger than the cap"); err != nil {
		t.Fatal(err)
	}
	events, _ = j2.Replay(0)
	if len(events) != 1 {
		t.Fatalf("newest event should always be retained, got %d", len(events))
	}
}

func TestJournalBoundedAfter10kPublishes(t *testing.T) {
	j := NewJournal(500, 64*1024)
	for i := 0; i < 10_000; i++ {
		if _, err := j.Publish(i); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := j.Replay(0)
	if len(events) > 500 {
		t.Fatalf("retained %d events, cap 500", len(events))
	}
	if events[len(events)-1].Seq != 10_000 {
		t.Fatalf("latest retained seq = %d, want 10000", events[len(events)-1].Seq)
	}
}

func TestJournalCloseSealsButKeepsReplay(t *testing.T) {
	j := NewJournal(100, 1<<20)
	if _, err := j.Publish("terminal"); err != nil {
		t.Fatal(err)
	}
	j.Close()
	if !j.Closed() {
		t.Fatal("Closed() = false")
	}
	if _, err := j.Publish("after close"); !errors.Is(err, ErrJournalClosed) {
		t.Fatalf("after close err = %v, want ErrJournalClosed", err)
	}
	events, latest := j.Replay(0)
	if latest != 1 || len(events) != 1 {
		t.Fatalf("replay after close: %+v latest=%d", events, latest)
	}
	// Late subscription still works for terminal state.
	initial, ch, _ := j.Subscribe(0)
	if len(initial) != 1 {
		t.Fatalf("late subscriber initial = %d events", len(initial))
	}
	j.Unsubscribe(ch)
}

func TestJournalConcurrent(t *testing.T) {
	j := NewJournal(1000, 1<<20)
	const publishers, per, subscribers = 10, 100, 4
	var wg sync.WaitGroup
	chans := make([]chan Event, subscribers)
	for i := range chans {
		_, ch, _ := j.Subscribe(0)
		chans[i] = ch
	}
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := j.Publish(i); err != nil {
					t.Errorf("publish: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	events, latest := j.Replay(0)
	if latest != publishers*per || len(events) != publishers*per {
		t.Fatalf("latest=%d retained=%d, want %d", latest, len(events), publishers*per)
	}
	for _, ch := range chans {
		j.Unsubscribe(ch)
	}
}
