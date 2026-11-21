package recovery

import (
	"testing"

	"mediaroute/internal/model"
	"mediaroute/internal/packet"
)

func TestRecoverBurst(t *testing.T) {
	r := New("v1", 1000, 3000)
	for seq := uint64(18); seq < 24; seq++ {
		r.AddParity(Parity{Protects: seq, Timestamp: r.TimestampFor(seq),
			Payload: []byte{byte(seq)}})
	}
	got, err := r.Recover([]packet.Gap{{From: 18, To: 23, Track: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("recovered %d, want 6", len(got))
	}
	for i, rec := range got {
		if rec.Seq != uint64(18+i) || rec.Timestamp != r.TimestampFor(rec.Seq) {
			t.Fatalf("bad recovered packet: %+v", rec)
		}
	}
}

func TestUnrecoverableGapContext(t *testing.T) {
	r := New("v1", 1000, 3000)
	r.AddParity(Parity{Protects: 5, Timestamp: r.TimestampFor(5), Payload: []byte{1}})
	_, err := r.Recover([]packet.Gap{{From: 5, To: 7, Track: "v1"}})
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrGap {
		t.Fatalf("want ErrGap, got %v", err)
	}
	if me.Track != "v1" || me.Seq != 6 || me.Timestamp != int64(r.TimestampFor(6)) {
		t.Fatalf("error missing track/seq/timestamp: %+v", me)
	}
	if model.ExitCode(me) != model.ExitUnrecoverable {
		t.Fatalf("exit code %d", model.ExitCode(me))
	}
}

func TestErrorPropagatesAcrossModules(t *testing.T) {
	r := New("a1", 0, 1920)
	_, err := r.Recover([]packet.Gap{{From: 9, To: 9, Track: "a1"}})
	me := err.(*model.MediaError)
	// same context travels through the sync stage
	w := me.With("sync", "cannot align across unrecovered gap")
	if w.Track != "a1" || w.Seq != 9 || w.Code != model.ErrGap {
		t.Fatalf("context lost across modules: %+v", w)
	}
	if model.ExitCode(w) != model.ExitUnrecoverable {
		t.Fatalf("exit code changed in propagation")
	}
}
