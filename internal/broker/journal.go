package broker

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
)

// ErrJournalClosed is returned by Publish after the journal has been sealed
// (e.g. an operation reached its terminal state).
var ErrJournalClosed = errors.New("journal closed")

// subscriberBufferSize mirrors the buffer size of plain Broker subscriber
// channels: a lagging consumer is dropped rather than stalling publishers.
const subscriberBufferSize = 32

// Event is one sequenced journal entry. Data is the JSON-encoded payload as
// supplied to Publish; Seq is assigned by the journal and is unique for the
// journal's lifetime.
type Event struct {
	Seq  int64
	Data []byte
}

// Journal is a bounded, ordered, replayable event stream.
//
// Unlike Broker (last-value cache per topic), a Journal keeps a sliding
// window of every published event so a subscriber can resume from the last
// sequence it has already seen (SSE Last-Event-ID semantics). Delivery is
// non-blocking: when a subscriber's buffer fills it is closed and removed,
// exactly like Broker's slow-consumer policy; the client reconnects and
// replays the gap from the retained window.
//
// The zero value is not usable; use NewJournal.
type Journal struct {
	mu       sync.Mutex
	buf      []Event // retained sliding window, oldest first
	total    int     // sum of len(Data) over buf
	nextSeq  int64   // seq assigned by the last Publish; 0 before the first
	stepCap  int     // maximum retained events
	bytesCap int     // maximum retained payload bytes
	closed   bool
	subs     []chan Event
}

// NewJournal returns a Journal retaining at most stepCap events and
// bytesCap payload bytes (the newest event is always retained even when it
// alone exceeds bytesCap). Non-positive caps are treated as 1.
func NewJournal(stepCap, bytesCap int) *Journal {
	if stepCap < 1 {
		stepCap = 1
	}
	if bytesCap < 1 {
		bytesCap = 1
	}
	return &Journal{stepCap: stepCap, bytesCap: bytesCap}
}

// Publish JSON-encodes data, assigns it the next sequence number, appends it
// to the retained window (evicting oldest entries to satisfy the caps) and
// delivers it to every current subscriber. Slow subscribers are dropped.
// After Close, Publish returns ErrJournalClosed.
func (j *Journal) Publish(data any) (Event, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return Event{}, ErrJournalClosed
	}
	j.nextSeq++
	ev := Event{Seq: j.nextSeq, Data: payload}
	j.buf = append(j.buf, ev)
	j.total += len(payload)
	j.trimLocked()
	j.deliverLocked(ev)
	return ev, nil
}

// trimLocked evicts oldest events until both caps are satisfied. The newest
// event is always kept. Caller must hold j.mu.
func (j *Journal) trimLocked() {
	for len(j.buf) > 1 && (len(j.buf) > j.stepCap || j.total > j.bytesCap) {
		j.total -= len(j.buf[0].Data)
		j.buf[0] = Event{} // help GC
		j.buf = j.buf[1:]
	}
}

// deliverLocked sends ev to every subscriber, closing and removing any whose
// buffer is full. Caller must hold j.mu.
func (j *Journal) deliverLocked(ev Event) {
	n := 0
	for _, ch := range j.subs {
		select {
		case ch <- ev:
			j.subs[n] = ch
			n++
		default:
			slog.Warn("journal: subscriber slow, closing stream", "seq", ev.Seq)
			close(ch)
		}
	}
	for i := n; i < len(j.subs); i++ {
		j.subs[i] = nil
	}
	j.subs = j.subs[:n]
}

// Replay returns the retained events with Seq strictly greater than afterSeq
// and the latest assigned seq (0 when nothing has been published). Callers
// compare afterSeq with OldestSeq to tell "nothing new" apart from
// "position older than the retained window".
func (j *Journal) Replay(afterSeq int64) (events []Event, latest int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	events = make([]Event, 0, len(j.buf))
	for _, ev := range j.buf {
		if ev.Seq > afterSeq {
			events = append(events, ev)
		}
	}
	return events, j.nextSeq
}

// OldestSeq returns the seq of the oldest still-retained event. It is 0 when
// nothing has been published, and equals LatestSeq when only one event
// remains. A resume position below OldestSeq means the gap has aged out and
// the caller needs a full snapshot rather than event replay.
func (j *Journal) OldestSeq() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.buf) == 0 {
		return 0
	}
	return j.buf[0].Seq
}

// Subscribe registers a subscriber and returns the retained events newer
// than afterSeq (the initial replay), the live event channel, and the
// latest assigned seq. Events published after subscription are delivered on
// the channel until it is closed (slow consumer) or Unsubscribe is called.
// Subscribing to a closed journal is allowed so late subscribers can read
// the terminal state via replay; use Closed to distinguish.
func (j *Journal) Subscribe(afterSeq int64) (initial []Event, ch chan Event, latest int64) {
	ch = make(chan Event, subscriberBufferSize)
	j.mu.Lock()
	defer j.mu.Unlock()
	initial = make([]Event, 0, len(j.buf))
	for _, ev := range j.buf {
		if ev.Seq > afterSeq {
			initial = append(initial, ev)
		}
	}
	j.subs = append(j.subs, ch)
	return initial, ch, j.nextSeq
}

// Unsubscribe removes ch and closes it. A channel already removed (slow
// consumer drop) is a no-op.
func (j *Journal) Unsubscribe(ch chan Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, c := range j.subs {
		if c == ch {
			j.subs = append(j.subs[:i], j.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Close seals the journal: further Publish calls fail with ErrJournalClosed.
// Existing subscribers are not closed — terminal delivery happens through
// the final event, and late subscribers keep replay access.
func (j *Journal) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
}

// Closed reports whether the journal has been sealed.
func (j *Journal) Closed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.closed
}
