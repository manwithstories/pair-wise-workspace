package rtp

import (
	"testing"
	"time"
)

// TestRetransmitHooksTerminate covers the retry paths that are easiest to get
// wrong: a hook that returns a useless packet, a hook that returns exactly the
// missing sequence forever, and a hook that never supplies one. In every case
// the bounded retry budget must let the run finish rather than spin, and the
// output must stay strictly ordered and duplicate-free.
func TestRetransmitHooksTerminate(t *testing.T) {
	cases := []struct {
		name string
		hook RetryHook
	}{
		{"stale packet", func(g Gap) (Packet, bool) { return Packet{Seq: 0}, true }},
		{"always the gap head", func(g Gap) (Packet, bool) { return Packet{Seq: g.First}, true }},
		{"no retransmit", func(g Gap) (Packet, bool) { return Packet{}, false }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewReorderer("v", 0, Config{Window: 2, MaxRetries: 3,
				MaxGapRun: 8, BufferBudget: 16})
			calls := 0
			hook := func(g Gap) (Packet, bool) {
				calls++
				if calls > 500 {
					t.Fatalf("hook invoked %d times; not making progress", calls)
				}
				return c.hook(g)
			}
			var out []Packet
			push := func(p Packet) {
				o, _, _ := r.Push(p, hook)
				out = append(out, o...)
			}
			push(Packet{Seq: 0})
			push(Packet{Seq: 2}) // leaves 1 missing
			for s := uint64(3); s <= 12; s++ {
				push(Packet{Seq: s})
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				o, _ := r.Close(hook)
				out = append(out, o...)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("Close did not terminate")
			}
			// Whatever the retransmit outcome, delivery must be strictly
			// increasing: no rewinds and no duplicates.
			for i := 1; i < len(out); i++ {
				if out[i].Seq <= out[i-1].Seq {
					t.Fatalf("delivery not strictly ordered at %d: %d then %d",
						i, out[i-1].Seq, out[i].Seq)
				}
			}
			if len(out) == 0 {
				t.Fatalf("no packets delivered")
			}
		})
	}
}
