// Package timeline implements event-time watermarks, bounded retraction, and
// deterministic replay. Events are buffered until the watermark advances past
// them. Late data inside the retraction horizon reopens the window and forces
// a recompute; data beyond the horizon is quarantined and blocks commit.
package timeline

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"sort"

	"mediapipe/internal/media"
)

// Config tunes watermark behaviour.
type Config struct {
	// MaxLateness is the ordering slack: the watermark trails the maximum
	// observed timestamp by this much.
	MaxLateness media.Timestamp
	// Retraction is the horizon within which late events reopen a window.
	// Anything older is quarantined.
	Retraction media.Timestamp
	// Window is the tumbling window size used for summaries.
	Window media.Timestamp
}

// DefaultConfig returns the delivery contract's values: 250 ms ordering slack
// and a 2 s retraction horizon.
func DefaultConfig() Config {
	return Config{
		MaxLateness: 250 * media.Millisecond,
		Retraction:  2 * media.Second,
		Window:      1 * media.Second,
	}
}

func (c Config) validate() error {
	if c.MaxLateness < 0 || c.Retraction < 0 || c.Window <= 0 {
		return media.NewError("timeline.config", media.ClassFatal,
			fmt.Errorf("invalid watermark config: %+v", c))
	}
	if c.Retraction < c.MaxLateness {
		return media.NewError("timeline.config", media.ClassFatal,
			fmt.Errorf("retraction %d is shorter than max lateness %d", c.Retraction, c.MaxLateness))
	}
	return nil
}

// Event is one timestamped observation entering the watermark engine.
type Event struct {
	// Key is the stable deduplication key. Two events with the same key are
	// the same observation even if they arrive twice.
	Key      string
	Track    uint64
	Time     media.Timestamp
	Duration media.Timestamp
	// Arrival is the wall-clock order of the event in the source. It only
	// breaks ties during deterministic replay.
	Arrival uint64
	// Payload identifies the content for digest purposes.
	Payload []byte
	// PTS is the presentation timestamp. It defaults to Time when zero and
	// lets the manifest record the delivered composition timeline.
	PTS media.Timestamp
	// Flags is a caller-defined bit set folded into replay digests.
	Flags uint32
}

// WindowSummary is one tumbling window's deterministic aggregate.
type WindowSummary struct {
	Start      media.Timestamp
	End        media.Timestamp
	Events     int
	Bytes      int64
	Digest     [32]byte
	Recomputed bool
}

// Quarantined records an event rejected for exceeding the retraction horizon.
type Quarantined struct {
	Event   Event
	Horizon media.Timestamp
}

// Stats describes engine activity for the manifest.
type Stats struct {
	Accepted     int
	Duplicates   int
	LateAccepted int
	Quarantined  int
	Recomputes   int
	Windows      int
}

// Engine is a single-pass watermark processor. It is not safe for concurrent
// use.
type Engine struct {
	cfg    Config
	digest hash.Hash

	maxSeen   media.Timestamp
	watermark media.Timestamp

	seen       map[string]struct{}
	windows    map[int64]*windowAccum
	order      []int64 // sorted window keys, maintained on demand
	quarantine []Quarantined
	stats      Stats
	replay     hash.Hash
}

type windowAccum struct {
	start      media.Timestamp
	events     []Event
	recomputed bool
}

// New creates an engine with the given config.
func New(cfg Config) (*Engine, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Engine{
		cfg:     cfg,
		digest:  sha256.New(),
		seen:    make(map[string]struct{}),
		windows: make(map[int64]*windowAccum),
		replay:  sha256.New(),
	}, nil
}

// Push accepts one event in arrival order. It returns true when the event was
// new, and whether it arrived late but within the retraction horizon. A
// quarantine triggers an error classified unsupported, which the caller turns
// into exit code 3 and a refused commit.
func (e *Engine) Push(ev Event) (accepted, late bool, err error) {
	if _, dup := e.seen[ev.Key]; dup {
		e.stats.Duplicates++
		return false, false, nil
	}
	e.seen[ev.Key] = struct{}{}
	e.writeReplay(ev)

	if ev.Time > e.maxSeen {
		e.maxSeen = ev.Time
	}
	candidate := e.maxSeen - e.cfg.MaxLateness
	if candidate < 0 {
		candidate = 0
	}
	if candidate > e.watermark {
		e.watermark = candidate
	}

	// A new event is late when its timestamp is already before the watermark,
	// which means the window it belongs to was (or would be) closed.
	if ev.Time < e.watermark {
		behind := e.watermark - ev.Time
		if behind > e.cfg.Retraction {
			q := Quarantined{Event: ev, Horizon: e.cfg.Retraction}
			e.quarantine = append(e.quarantine, q)
			e.stats.Quarantined++
			return false, true, media.AtTrack("timeline.watermark", -1, ev.Track,
				media.ClassUnsupported,
				"event %q at %d is %d behind watermark, beyond retraction horizon %d",
				ev.Key, ev.Time, behind, e.cfg.Retraction)
		}
		e.stats.LateAccepted++
		late = true
	} else {
		e.stats.Accepted++
	}

	key := int64(ev.Time/e.cfg.Window) * int64(e.cfg.Window)
	w, ok := e.windows[key]
	if !ok {
		w = &windowAccum{start: media.Timestamp(key)}
		e.windows[key] = w
		e.order = nil
	}
	if late && w.recomputed == false {
		w.recomputed = true
		e.stats.Recomputes++
	}
	w.events = append(w.events, ev)
	return true, late, nil
}

// Watermark returns the current event-time watermark.
func (e *Engine) Watermark() media.Timestamp { return e.watermark }

// Quarantine returns the rejected events in arrival order.
func (e *Engine) Quarantine() []Quarantined { return append([]Quarantined(nil), e.quarantine...) }

// Stats returns a snapshot of counters.
func (e *Engine) Stats() Stats {
	s := e.stats
	s.Windows = len(e.windows)
	return s
}

// ReplayDigest returns the digest of every accepted event in arrival order,
// plus the final watermark. Re-running the same input yields the same value;
// duplicates are excluded because they were rejected as repeats.
func (e *Engine) ReplayDigest() [32]byte {
	var out [32]byte
	copy(out[:], e.replay.Sum(nil))
	return out
}

func (e *Engine) writeReplay(ev Event) {
	var buf [8]byte
	h := e.replay
	writeField(h, []byte(ev.Key))
	binary.BigEndian.PutUint64(buf[:], ev.Track)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(ev.Time))
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(ev.Duration))
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], ev.Arrival)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(ev.PTS))
	h.Write(buf[:])
	binary.BigEndian.PutUint32(buf[:4], ev.Flags)
	h.Write(buf[:4])
	writeField(h, ev.Payload)
}

func writeField(h hash.Hash, b []byte) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(b)))
	h.Write(buf[:])
	h.Write(b)
}

func (e *Engine) sortedKeys() []int64 {
	if e.order != nil {
		return e.order
	}
	keys := make([]int64, 0, len(e.windows))
	for k := range e.windows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	e.order = keys
	return keys
}

// Summaries returns every window's aggregate in ascending start order. Events
// inside a window are sorted by (Time, Key) so a recompute produces the same
// digest regardless of arrival order.
func (e *Engine) Summaries() []WindowSummary {
	keys := e.sortedKeys()
	out := make([]WindowSummary, 0, len(keys))
	for _, k := range keys {
		w := e.windows[k]
		events := append([]Event(nil), w.events...)
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].Time != events[j].Time {
				return events[i].Time < events[j].Time
			}
			return events[i].Key < events[j].Key
		})
		d := sha256.New()
		var sum int64
		for _, ev := range events {
			writeField(d, []byte(ev.Key))
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(ev.Time))
			d.Write(b[:])
			binary.BigEndian.PutUint64(b[:], ev.Track)
			d.Write(b[:])
			writeField(d, ev.Payload)
			sum += int64(len(ev.Payload))
		}
		var digest [32]byte
		copy(digest[:], d.Sum(nil))
		out = append(out, WindowSummary{
			Start:      w.start,
			End:        w.start + e.cfg.Window,
			Events:     len(events),
			Bytes:      sum,
			Digest:     digest,
			Recomputed: w.recomputed,
		})
	}
	return out
}

// Timeline returns the first window start and the last closed end, which is
// what the manifest reports as the delivered time range.
func (e *Engine) Timeline() (start, end media.Timestamp) {
	keys := e.sortedKeys()
	if len(keys) == 0 {
		return 0, 0
	}
	start = e.windows[keys[0]].start
	for _, k := range keys {
		w := e.windows[k]
		for _, ev := range w.events {
			if end2 := ev.Time + maxDur(ev.Duration); end2 > end {
				end = end2
			}
		}
		if w.start+e.cfg.Window > end {
			end = w.start + e.cfg.Window
		}
	}
	return start, end
}

func maxDur(d media.Timestamp) media.Timestamp {
	if d < 0 {
		return 0
	}
	return d
}

// SummaryDigest folds all window digests plus the final watermark into one
// value. It is the replay fingerprint recorded in the manifest.
func (e *Engine) SummaryDigest() [32]byte {
	h := sha256.New()
	for _, s := range e.Summaries() {
		h.Write(s.Digest[:])
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(s.End))
		h.Write(b[:])
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(e.watermark))
	h.Write(b[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// HexDigest formats a digest for terminal output and manifests.
func HexDigest(d [32]byte) string { return fmt.Sprintf("%x", d[:]) }
