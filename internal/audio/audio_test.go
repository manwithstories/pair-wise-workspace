package audio

import (
	"testing"

	"github.com/pion/rtp"

	"mediatool/internal/clock"
)

func TestMapSamplesExplicit(t *testing.T) {
	st, _ := Lookup("stereo")
	mono, _ := Lookup("mono")
	in := []float32{1, 2, 3, 4} // 2 帧立体声
	out, fault := MapSamples(in, st, mono, []int{0})
	if fault != nil {
		t.Fatalf("fault: %v", fault)
	}
	if len(out) != 2 || out[0] != 1 || out[1] != 3 {
		t.Fatalf("out = %v", out)
	}
}

func TestMapSamplesMissingMapping(t *testing.T) {
	st, _ := Lookup("stereo")
	sur, _ := Lookup("5.1")
	_, fault := MapSamples(make([]float32, 8), st, sur, nil)
	if fault == nil || fault.Kind != clock.FaultChannelMapMissing {
		t.Fatalf("want channel-map-missing fault, got %v", fault)
	}
}

func TestMapSamplesIdentity(t *testing.T) {
	st, _ := Lookup("stereo")
	in := []float32{1, 2, 3, 4}
	out, fault := MapSamples(in, st, st, nil)
	if fault != nil || len(out) != 4 {
		t.Fatalf("identity mapping failed: %v %v", out, fault)
	}
}

func TestResampleLinearRatio(t *testing.T) {
	in := make([]float32, 480) // 480 个采样 @48k = 10ms
	out := ResampleLinear(in, 48000, 44100)
	if len(out) != 441 {
		t.Fatalf("len = %d, want 441", len(out))
	}
	// 同率直通
	if got := ResampleLinear(in, 48000, 48000); len(got) != 480 {
		t.Fatalf("same-rate len = %d", len(got))
	}
}

func TestResampleBoundaries(t *testing.T) {
	in := make([]float32, 960) // 前 480 @48k，后 480 @44.1k
	out, fault := ResampleWithBoundaries(in, 48000,
		[]RateBoundary{{AtSample: 480, NewRate: 44100}}, 48000)
	if fault == nil || fault.Kind != clock.FaultAudioRateSwitch || !fault.Recovered {
		t.Fatalf("fault = %v", fault)
	}
	want := 480 + 480*48000/44100
	if len(out) != want {
		t.Fatalf("len = %d, want %d", len(out), want)
	}
}

func rtpBytes(seq uint16, pt uint8, ts uint32) []byte {
	p := rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, PayloadType: pt},
		Payload: []byte{0xAA, 0xBB},
	}
	raw, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

func TestRTPSourceLossAndRateSwitch(t *testing.T) {
	src := NewRTPSource(map[uint8]int{96: 48000, 97: 44100})
	if _, f, err := src.Ingest(rtpBytes(10, 96, 0)); err != nil || f != nil {
		t.Fatalf("first: %v %v", f, err)
	}
	res, f, err := src.Ingest(rtpBytes(13, 96, 960)) // 丢 11,12
	if err != nil || f == nil || f.Kind != clock.FaultPacketLoss || res.Lost != 2 {
		t.Fatalf("gap: res=%+v fault=%v err=%v", res, f, err)
	}
	if _, _, err := src.Ingest(rtpBytes(14, 97, 1440)); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if len(src.SwitchPts) != 1 || src.SwitchPts[0] != 2 {
		t.Fatalf("switch points = %v", src.SwitchPts)
	}
}

func TestRTPUnknownPT(t *testing.T) {
	src := NewRTPSource(map[uint8]int{96: 48000})
	_, f, err := src.Ingest(rtpBytes(1, 55, 0))
	if err != nil || f == nil || f.Kind != clock.FaultAudioRateSwitch {
		t.Fatalf("fault=%v err=%v", f, err)
	}
}
