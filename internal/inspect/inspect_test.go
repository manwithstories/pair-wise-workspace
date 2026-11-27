package inspect

import (
	"strings"
	"testing"

	"mediacheck/internal/clock"
	"mediacheck/internal/model"
	"mediacheck/internal/opus"
)

func audioPacket(seq uint32, data []byte, samples int) model.Packet {
	tb := clock.Audio(48000)
	start := int64(seq) * tb.Nanos(int64(samples))
	end := start + tb.Nanos(int64(samples))
	return model.Packet{
		Track: model.TrackAudio, Codec: model.CodecOpus, Sequence: seq,
		PTS: int64(seq) * int64(samples), Duration: int64(samples),
		Timebase: tb, Slot: model.Slot{Start: start, End: end},
		Data: data, Keyframe: true,
	}
}

func realOpusPackets(t *testing.T, n, samples int) [][]byte {
	t.Helper()
	enc, err := opus.NewEncoder(48000, 2, 24000, opus.ApplicationVoIP, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var out [][]byte
	for i := 0; i < n; i++ {
		p := make([]int16, samples*2)
		for j := range p {
			p[j] = int16((i*samples + j) % 1000)
		}
		data, err := enc.Encode(p, samples)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
	return out
}

func TestParseOpusDurationMatchesEncoder(t *testing.T) {
	for _, samples := range []int{120, 240, 480, 960, 1920} {
		pkts := realOpusPackets(t, 1, samples)
		got, err := ParseOpusDuration(pkts[0])
		if err != nil {
			t.Fatalf("ParseOpusDuration(%d): %v", samples, err)
		}
		if got != samples {
			t.Fatalf("declared duration for %d-sample frame = %d", samples, got)
		}
	}
}

func TestParseOpusDurationRejectsTruncationAndBadTOC(t *testing.T) {
	if _, err := ParseOpusDuration(nil); err == nil {
		t.Fatal("expected error for empty packet")
	}
	// A single TOC byte means the frame data was truncated away.
	if _, err := ParseOpusDuration([]byte{0x00}); err == nil {
		t.Fatal("expected truncated header error for a TOC-only packet")
	}
	// Code 3 needs a frame count byte.
	if _, err := ParseOpusDuration([]byte{0 | 3}); err == nil {
		t.Fatal("expected error for missing frame count")
	}
	if _, err := ParseOpusDuration([]byte{0 | 3, 0}); err == nil {
		t.Fatal("expected error for zero frame count")
	}
}

func TestAACADTSHeaderParsing(t *testing.T) {
	// Build a stereo, 48 kHz, 1024-sample ADTS frame of 100 bytes.
	data := make([]byte, 100)
	data[0] = 0xFF
	data[1] = 0xF1
	data[2] = byte((1 << 6) | (3 << 2)) // profile LC, sfi 3 = 48 kHz
	data[3] = byte((2 << 6) | 0x00)
	frameLen := 100
	data[3] |= byte(frameLen>>11) & 0x03
	data[4] = byte(frameLen >> 3)
	data[5] = byte((frameLen & 0x07) << 5)
	data[6] = 0xFC
	samples, gotLen, err := ParseADTSDuration(data)
	if err != nil {
		t.Fatalf("ParseADTSDuration: %v", err)
	}
	if gotLen != 100 {
		t.Fatalf("frame length = %d, want 100", gotLen)
	}
	if samples != 1024 {
		t.Fatalf("samples = %d, want 1024 at 48kHz", samples)
	}
	// Declared length beyond the buffer is a bad-length fault.
	bad := append([]byte(nil), data[:20]...)
	if _, _, err := ParseADTSDuration(bad); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected bad length error, got %v", err)
	}
	// A short buffer is a truncated header.
	if _, _, err := ParseADTSDuration([]byte{0xFF, 0xF1}); err == nil {
		t.Fatal("expected truncated header error")
	}
}

func TestInspectorCleanRunHasNoDamage(t *testing.T) {
	pkts := realOpusPackets(t, 10, 960)
	in := New()
	for i, data := range pkts {
		in.Inspect(audioPacket(uint32(i), data, 960))
	}
	res := in.Finish()
	if len(res.Faults) != 0 {
		t.Fatalf("clean stream produced faults: %v", FaultText(res.Faults, 5))
	}
	if res.Streams[model.TrackAudio].Packets != 10 {
		t.Fatalf("packets = %d", res.Streams[model.TrackAudio].Packets)
	}
}

func TestInspectorTruncatedPacketDoesNotHideLaterPackets(t *testing.T) {
	pkts := realOpusPackets(t, 6, 960)
	in := New()
	for i, data := range pkts {
		if i == 2 {
			data = data[:1] // keep only the TOC byte; frame data is gone
		}
		out := in.Inspect(audioPacket(uint32(i), data, 960))
		if out.Sequence != uint32(i) {
			t.Fatalf("packet %d was not passed through", i)
		}
	}
	res := in.Finish()
	if len(res.Faults) == 0 {
		t.Fatal("expected a fault for the truncated packet")
	}
	if res.Damage.Len() != 1 {
		t.Fatalf("damaged ranges = %d, want 1: %v", res.Damage.Len(), res.Damage.Ranges())
	}
	if res.Streams[model.TrackAudio].Packets != 6 {
		t.Fatalf("inspector saw %d packets, want 6", res.Streams[model.TrackAudio].Packets)
	}
}

func TestInspectorReportsTruncatedAACAndVideoWithoutLosingPackets(t *testing.T) {
	in := New()
	// AAC packet shorter than its 7 byte header.
	in.Inspect(model.Packet{Track: model.TrackAudio, Codec: model.CodecAAC, Sequence: 0, Duration: 1024,
		Timebase: clock.Audio(48000), Slot: model.Slot{Start: 0, End: 1}, Data: []byte{0xFF, 0xF1}})
	// H.264 access unit whose length prefix promises more than it carries.
	bad := []byte{0x00, 0x00, 0x10, 0x00, 0x65, 0x88}
	in.Inspect(model.Packet{Track: model.TrackVideo, Codec: model.CodecH264, Sequence: 0, Duration: 1,
		Timebase: clock.Video(25, 1), Slot: model.Slot{Start: 0, End: 40_000_000}, Data: bad})
	res := in.Finish()
	if len(res.Faults) != 2 {
		t.Fatalf("faults = %d, want 2: %v", len(res.Faults), FaultText(res.Faults, 5))
	}
	if res.Streams[model.TrackAudio].Packets != 1 || res.Streams[model.TrackVideo].Packets != 1 {
		t.Fatal("damaged packets were dropped instead of reported")
	}
}

func TestKeyframeIntervals(t *testing.T) {
	in := New()
	tb := clock.Video(25, 1)
	for i := 0; i < 100; i++ {
		start := tb.Nanos(int64(i))
		in.Inspect(model.Packet{
			Track: model.TrackVideo, Codec: model.CodecH264, Sequence: uint32(i),
			PTS: int64(i), Duration: 1, Timebase: tb,
			Slot:     model.Slot{Start: start, End: start + tb.Nanos(1)},
			Keyframe: i%25 == 0,
			Data:     []byte{0, 0, 0, 2, 0x65, 0x88},
		})
	}
	res := in.Finish()
	st := res.Streams[model.TrackVideo]
	if st.Keyframes != 4 {
		t.Fatalf("keyframes = %d, want 4", st.Keyframes)
	}
	if st.MaxGapNanos != 1_000_000_000 || st.MinGapNanos != 1_000_000_000 {
		t.Fatalf("keyframe gap = %d..%d ns, want 1s", st.MinGapNanos, st.MaxGapNanos)
	}
	if got := st.KeyframeInterval(); got != 1000 {
		t.Fatalf("average interval = %v ms, want 1000", got)
	}
}

func TestH264NALHelpers(t *testing.T) {
	// IDR slice then SEI.
	au := []byte{0, 0, 0, 3, 0x65, 1, 2, 0, 0, 0, 2, 0x06, 0xAA}
	types, err := H264NALTypes(au)
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 2 || types[0] != 5 || types[1] != 6 {
		t.Fatalf("types = %v", types)
	}
	key, err := IsH264Keyframe(au)
	if err != nil || !key {
		t.Fatalf("keyframe = %v, %v", key, err)
	}
	if _, err := H264NALTypes([]byte{0, 0, 0, 9, 0x41}); err == nil {
		t.Fatal("expected bad length error")
	}
	if _, err := H264NALTypes([]byte{0, 0, 0}); err == nil {
		t.Fatal("expected truncated header error")
	}
}

func TestDurationChangesAreInformational(t *testing.T) {
	in := New()
	for i, dur := range []int64{960, 960, 480, 960} {
		in.Inspect(model.Packet{
			Track: model.TrackAudio, Codec: model.CodecOpus, Sequence: uint32(i),
			Duration: dur, Timebase: clock.Audio(48000),
			Slot: model.Slot{Start: int64(i) * 20_000_000, End: int64(i+1) * 20_000_000},
			Data: []byte{0x00, 0xFF},
		})
	}
	res := in.Finish()
	if len(res.AudioModes[model.TrackAudio]) != 2 {
		t.Fatalf("duration changes = %d, want 2", len(res.AudioModes[model.TrackAudio]))
	}
	if res.Damage.Len() != 0 {
		t.Fatal("duration changes must not create damage")
	}
}

func TestValidateStreamInfo(t *testing.T) {
	good := model.StreamInfo{Track: model.TrackAudio, Codec: model.CodecOpus, Timebase: clock.Audio(48000), SampleRate: 48000, Channels: 2}
	if err := ValidateStreamInfo(good); err != nil {
		t.Fatalf("valid stream rejected: %v", err)
	}
	bad := good
	bad.Timebase = clock.Timebase{}
	if err := ValidateStreamInfo(bad); err == nil {
		t.Fatal("expected invalid timebase error")
	}
	bad = good
	bad.Channels = 0
	if err := ValidateStreamInfo(bad); err == nil {
		t.Fatal("expected missing channel error")
	}
}
