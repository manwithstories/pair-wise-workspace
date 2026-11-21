package video

import "testing"

func mkPayload(id uint16, flags byte, cto uint32) []byte {
	p := make([]byte, headerLen+3)
	p[0], p[1] = byte(id>>8), byte(id)
	p[2] = flags
	p[3], p[4], p[5], p[6] = byte(cto>>24), byte(cto>>16), byte(cto>>8), byte(cto)
	return p
}

func buildFrames(t *testing.T, in []PacketInput) []*Frame {
	t.Helper()
	b := NewBuilder()
	var frames []*Frame
	for _, p := range in {
		f, err := b.Push(p)
		if err != nil {
			t.Fatalf("push tick=%d: %v", p.Tick, err)
		}
		if f != nil {
			frames = append(frames, f)
		}
	}
	return frames
}

func TestFrameBoundaries(t *testing.T) {
	// Frame 0 spans two packets (marker closes it); frame 1 is one packet.
	in := []PacketInput{
		{Tick: 0, SSRC: 1, Seq: 10, Timestamp: 0, Payload: mkPayload(0, flagKeyframe, 0)},
		{Tick: 1, SSRC: 1, Seq: 11, Timestamp: 0, Marker: true, Payload: mkPayload(0, flagKeyframe, 0)},
		{Tick: 2, SSRC: 1, Seq: 12, Timestamp: 3000, Marker: true, Payload: mkPayload(1, 0, 0)},
	}
	frames := buildFrames(t, in)
	if len(frames) != 2 {
		t.Fatalf("frames: %d", len(frames))
	}
	if len(frames[0].Packets) != 2 || !frames[0].Keyframe || frames[0].FirstTick != 0 {
		t.Fatalf("frame0: %+v", frames[0])
	}
	dts1 := float64(3000) / float64(ClockRate) * float64(1e6)
	wantDTS1 := int64(dts1 + 0.5)
	if frames[0].DTSUs != 0 || frames[1].DTSUs != wantDTS1 {
		t.Fatalf("DTS mapping: %d %d", frames[0].DTSUs, frames[1].DTSUs)
	}
}

func TestVFR(t *testing.T) {
	// Non-uniform RTP timestamp gaps: 33.3 ms, then 40 ms, then 20 ms.
	ts := []uint32{0, 3000, 6600, 8400}
	var in []PacketInput
	for i, v := range ts {
		in = append(in, PacketInput{
			Tick: int64(i), SSRC: 2, Seq: uint16(i), Timestamp: v,
			Marker: true, Payload: mkPayload(uint16(i), 0, 0),
		})
	}
	frames := buildFrames(t, in)
	dts, pts := Axes(frames)
	if len(dts.Frames) != 4 || len(pts.Frames) != 4 {
		t.Fatalf("axes: %d %d", len(dts.Frames), len(pts.Frames))
	}
	got := Durations(pts)
	want := []int64{0, 33333, 40000, 20000}
	for i := range want {
		if abs64(got[i]-want[i]) > 5 {
			t.Fatalf("VFR duration[%d]: got %d want %d", i, got[i], want[i])
		}
	}
}

func TestPTSReorder(t *testing.T) {
	// Decode order 0,1,2 but frame 1 has a positive composition offset, so
	// presentation order becomes 0,2,1.
	in := []PacketInput{
		{Tick: 0, SSRC: 3, Seq: 0, Timestamp: 0, Marker: true, Payload: mkPayload(0, flagKeyframe, 0)},
		{Tick: 1, SSRC: 3, Seq: 1, Timestamp: 3000, Marker: true, Payload: mkPayload(1, 0, 6000)},
		{Tick: 2, SSRC: 3, Seq: 2, Timestamp: 6000, Marker: true, Payload: mkPayload(2, 0, 0)},
	}
	frames := buildFrames(t, in)
	dts, pts := Axes(frames)
	if dts.Frames[0].ID != 0 || dts.Frames[1].ID != 1 || dts.Frames[2].ID != 2 {
		t.Fatal("DTS axis must follow decode order")
	}
	ids := []uint32{pts.Frames[0].ID, pts.Frames[1].ID, pts.Frames[2].ID}
	want := []uint32{0, 2, 1}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("PTS order: got %v want %v", ids, want)
		}
	}
}

func TestBadFrameIsolation(t *testing.T) {
	in := []PacketInput{
		{Tick: 0, SSRC: 4, Seq: 0, Timestamp: 0, Marker: true, Payload: mkPayload(0, flagKeyframe, 0)},
		{Tick: 1, SSRC: 4, Seq: 1, Timestamp: 3000, Marker: true, Payload: mkPayload(1, flagBad, 0)},
		{Tick: 2, SSRC: 4, Seq: 2, Timestamp: 6000, Marker: true, Payload: mkPayload(2, flagKeyframe, 0)},
	}
	frames := buildFrames(t, in)
	if len(frames) != 3 || !frames[1].Bad {
		t.Fatalf("bad frame not isolated: %+v", frames)
	}
	if !frames[2].Keyframe {
		t.Fatal("keyframe after a bad frame must remain indexable")
	}
}

func TestShortPayload(t *testing.T) {
	b := NewBuilder()
	if _, err := b.Push(PacketInput{Tick: 0, SSRC: 1, Payload: []byte{0, 1}}); err == nil {
		t.Fatal("truncated payload must error")
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
