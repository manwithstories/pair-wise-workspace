package audio

import (
	"math"
	"testing"

	"mediapipeline/internal/mediaerr"
)

func TestLoudnessWindowCarryAndTail(t *testing.T) {
	m, merr := NewLoudnessMeter(4)
	if merr != nil {
		t.Fatal(merr)
	}
	// First block of 5 completes one window and carries one sample.
	ws, merr := m.Process([]float64{1, 1, 1, 1, 0.5})
	if merr != nil || len(ws) != 1 {
		t.Fatalf("window output wrong: %+v err=%v", ws, merr)
	}
	if math.Abs(ws[0].RMS-1) > 1e-12 || ws[0].Peak != 1 || ws[0].StartSample != 0 {
		t.Fatalf("full window stats wrong: %+v", ws[0])
	}
	// Two more samples leave a sub-window tail of 3 (one carried + two).
	ws, _ = m.Process([]float64{0.5, 0.5})
	if len(ws) != 0 {
		t.Fatalf("incomplete window must be carried: %+v", ws)
	}
	tail := m.Flush()
	if tail.Length != 3 || tail.StartSample != 4 {
		t.Fatalf("sub-window tail wrong: %+v", tail)
	}
}

func TestLoudnessSilenceAndBurst(t *testing.T) {
	m, _ := NewLoudnessMeter(3)
	ws, _ := m.Process([]float64{0, 0, 0})
	if ws[0].LoudnessDB != silenceDB || ws[0].RMS != 0 || ws[0].Peak != 0 {
		t.Fatalf("silence window must be stable: %+v", ws[0])
	}
	ws, _ = m.Process([]float64{1, 0, 0}) // burst peak with low RMS
	if ws[0].Peak != 1 || math.Abs(ws[0].RMS-1/math.Sqrt(3)) > 1e-15 {
		t.Fatalf("burst window wrong: %+v", ws[0])
	}
	wantDB := 20 * math.Log10(1/math.Sqrt(3))
	if math.Abs(ws[0].LoudnessDB-wantDB) > 1e-12 {
		t.Fatalf("dBFS wrong: got %f want %f", ws[0].LoudnessDB, wantDB)
	}
}

func TestLoudnessEmptyStreamFlush(t *testing.T) {
	m, _ := NewLoudnessMeter(8)
	if ws, merr := m.Process(nil); merr != nil || ws != nil {
		t.Fatalf("empty block must be no-op: %v err=%v", ws, merr)
	}
	tail := m.Flush()
	if tail.Length != 0 || tail.LoudnessDB != silenceDB {
		t.Fatalf("empty stream flush must be stable: %+v", tail)
	}
}

func TestLoudnessReuseAfterFlush(t *testing.T) {
	m, _ := NewLoudnessMeter(2)
	m.Process([]float64{0.1, 0.2, 0.3})
	m.Flush()
	ws, _ := m.Process([]float64{0.4, 0.4})
	if len(ws) != 1 || ws[0].StartSample != 0 {
		t.Fatalf("meter must restart cleanly after flush: %+v", ws)
	}
}

func TestLimiterPeakCeiling(t *testing.T) {
	l, merr := NewLimiter(0.5)
	if merr != nil {
		t.Fatal(merr)
	}
	out, gain := l.Limit([]float64{1, -2, 0.25})
	if math.Abs(gain-0.25) > 1e-12 {
		t.Fatalf("gain wrong: %f", gain)
	}
	want := []float64{0.25, -0.5, 0.0625}
	for i := range want {
		if math.Abs(out[i]-want[i]) > 1e-12 {
			t.Fatalf("limited output wrong: %v want %v", out, want)
		}
	}
	for _, v := range out {
		if math.Abs(v) > 0.5+1e-12 {
			t.Fatalf("ceiling breached: %v", out)
		}
	}
}

func TestLimiterPassThrough(t *testing.T) {
	l, _ := NewLimiter(0.9)
	in := []float64{0.3, -0.8, 0}
	out, gain := l.Limit(in)
	if gain != 1 {
		t.Fatalf("sub-ceiling block must not be scaled: gain=%f", gain)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("pass-through changed samples: %v", out)
		}
	}
	if o, g := l.Limit(nil); len(o) != 0 || g != 1 {
		t.Fatalf("empty block handling wrong: %v %f", o, g)
	}
}

func TestLimiterInvalidCeiling(t *testing.T) {
	if _, merr := NewLimiter(0); merr == nil || merr.Kind != mediaerr.KindInvalidInput {
		t.Fatal("zero ceiling must error")
	}
	if _, merr := NewLimiter(1.5); merr == nil {
		t.Fatal("ceiling above 1 must error")
	}
	if _, merr := NewLoudnessMeter(0); merr == nil {
		t.Fatal("zero window must error")
	}
}
