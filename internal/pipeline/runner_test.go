package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
)

// fakeRenderer produces deterministic bytes from the merged event count.
type fakeRenderer struct{ calls int32 }

func (f *fakeRenderer) Render(_ context.Context, seg *Segment) ([]byte, string, error) {
	atomic.AddInt32(&f.calls, 1)
	var b strings.Builder
	for _, lane := range seg.Lanes {
		for _, ev := range lane.Events {
			b.WriteString(ev.Text)
			b.WriteByte('|')
		}
	}
	payload := []byte(b.String())
	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:]), nil
}

// memStore records committed payloads and can simulate write failures.
type memStore struct {
	written   map[int][]byte
	manifest  map[int]string
	failAfter int
	writes    int32
}

func newMemStore() *memStore {
	return &memStore{written: map[int][]byte{}, manifest: map[int]string{}}
}

func (m *memStore) HasMatchingOutput(id int, inputHash string) (bool, error) {
	h, ok := m.manifest[id]
	return ok && h == inputHash && inputHash != "", nil
}

func (m *memStore) WriteSegment(seg *Segment, payload []byte) error {
	if m.failAfter > 0 && int(atomic.AddInt32(&m.writes, 1)) > m.failAfter {
		return errors.New("disk full")
	}
	cp := append([]byte(nil), payload...)
	m.written[seg.ID] = cp
	m.manifest[seg.ID] = seg.InputHash
	return nil
}

func fixtureSources(n int) []SubtitleSource {
	var b strings.Builder
	for i := 0; i < n; i++ {
		start := (i * 500) % 9000 // ms, spans several 3 s windows
		end := start + 400
		b.WriteString("1\n")
		b.WriteString(stamp(start))
		b.WriteString(" --> ")
		b.WriteString(stamp(end))
		b.WriteString("\nline-")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n\n")
	}
	return []SubtitleSource{{
		Track: "track-a", Source: "in.srt",
		Reader: bytes.NewReader([]byte(b.String())),
	}}
}

func sources(t *testing.T, n int) []SubtitleSource {
	if t != nil {
		t.Helper()
	}
	return fixtureSources(n)
}

func stamp(ms int) string {
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	ms %= 1000
	return strings.Repeat("0", 2-len(itoa(h))) + itoa(h) + ":" +
		pad2(m) + ":" + pad2(s) + "," + pad3(ms)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
func pad2(n int) string {
	s := itoa(n)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}
func pad3(n int) string {
	s := itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func baseCfg(store SegmentStore) Config {
	return Config{
		QueueCap: 2, MemoryBudget: DefaultMemoryBudget,
		SegmentTicks: clock.MillisToTick(3000),
		Horizon:      clock.MillisToTick(60_000),
		Output:       store,
	}
}

func TestRunHappyPath(t *testing.T) {
	store := newMemStore()
	cfg := baseCfg(store)
	// Give every segment a non-empty input hash via a small wrapping store.
	renderer := &fakeRenderer{}
	coll := diag.NewCollector(0)
	results, err := Run(context.Background(), sources(t, 12), cfg, renderer, coll)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("segments=%d want >=2", len(results))
	}
	total := 0
	for _, r := range results {
		if len(r.Segment.OutputHash) != 64 {
			t.Fatalf("segment %d hash len=%d", r.Segment.ID, len(r.Segment.OutputHash))
		}
		if _, ok := store.written[r.Segment.ID]; !ok {
			t.Fatalf("segment %d not persisted", r.Segment.ID)
		}
		total += r.Segment.Summary.Accepted
	}
	if total != 12 {
		t.Fatalf("accepted events=%d want 12", total)
	}
}

func TestSlowConsumerStillCompletes(t *testing.T) {
	store := newMemStore()
	cfg := baseCfg(store)
	var stalls int32
	cfg.SlowConsumer = func() {
		atomic.AddInt32(&stalls, 1)
		time.Sleep(2 * time.Millisecond)
	}
	coll := diag.NewCollector(0)
	results, err := Run(context.Background(), sources(t, 10), cfg, &fakeRenderer{}, coll)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stalls == 0 || len(results) == 0 {
		t.Fatal("slow-consumer hook not exercised")
	}
}

func TestCancellationStopsProduction(t *testing.T) {
	store := newMemStore()
	cfg := baseCfg(store)
	cfg.SlowConsumer = func() { time.Sleep(50 * time.Millisecond) }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	coll := diag.NewCollector(0)
	start := time.Now()
	_, err := Run(ctx, sources(t, 200), cfg, &fakeRenderer{}, coll)
	if err == nil {
		t.Fatal("cancelled run returned nil error")
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("error type=%T want *RunError", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("pipeline did not unwind promptly after cancel")
	}
}

func TestWriteFailureKeepsCommitted(t *testing.T) {
	store := newMemStore()
	store.failAfter = 1 // second WriteSegment fails
	cfg := baseCfg(store)
	coll := diag.NewCollector(0)
	results, err := Run(context.Background(), sources(t, 12), cfg, &fakeRenderer{}, coll)
	if err == nil {
		t.Fatal("write failure not surfaced")
	}
	// Exactly the segments committed before the failure are returned.
	if len(results) != len(store.written) {
		t.Fatalf("committed=%d persisted=%d mismatch", len(results), len(store.written))
	}
	for _, r := range results {
		if _, ok := store.written[r.Segment.ID]; !ok {
			t.Fatalf("uncommitted segment %d reported as result", r.Segment.ID)
		}
	}
}

func TestResumeSkipsMatchingSegments(t *testing.T) {
	store := newMemStore()
	cfg := baseCfg(store)
	renderer := &fakeRenderer{}
	coll := diag.NewCollector(0)

	// First run with a freshly built reader set.
	first, err := Run(context.Background(), sources(t, 9), cfg, renderer, coll)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a previous manifest by seeding input hashes.
	for _, r := range first {
		store.manifest[r.Segment.ID] = "fixed-input-hash"
	}

	// A store whose matching rule always returns true for seeded ids forces
	// the resume path without coupling to input-hash plumbing here.
	skipStore := &alwaysMatchStore{ids: store.manifest}
	cfg.Output = skipStore
	second, err := Run(context.Background(), sources(t, 9), cfg, renderer, diag.NewCollector(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range second {
		if !r.Segment.Skipped {
			t.Fatalf("segment %d was recomputed instead of skipped", r.Segment.ID)
		}
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	coll := diag.NewCollector(0)
	if _, err := Run(context.Background(), sources(t, 1), Config{}, &fakeRenderer{}, coll); err == nil {
		t.Fatal("zero-value config accepted")
	}
}

type alwaysMatchStore struct{ ids map[int]string }

func (a *alwaysMatchStore) HasMatchingOutput(id int, _ string) (bool, error) {
	_, ok := a.ids[id]
	return ok, nil
}
func (a *alwaysMatchStore) WriteSegment(_ *Segment, _ []byte) error { return nil }
