package ts

import "testing"

// nal builds an Annex-B NAL unit with the 4-byte start code.
func nal(header byte, payloadLen int) []byte {
	b := []byte{0x00, 0x00, 0x00, 0x01, header}
	for i := 0; i < payloadLen; i++ {
		b = append(b, byte(i&0x3F))
	}
	return b
}

func TestVideoAssemblerAccessUnits(t *testing.T) {
	va := NewVideoAssembler(StreamTypeH264)
	// Unit 1: AUD + SPS + PPS + IDR slice
	stream := append([]byte{}, nal(0x09, 5)...) // AUD
	stream = append(stream, nal(0x67, 10)...)    // SPS (type 7)
	stream = append(stream, nal(0x68, 8)...)     // PPS (type 8)
	stream = append(stream, nal(0x65, 40)...)    // IDR slice (type 5)
	// Unit 2: AUD + non-IDR slice
	stream = append(stream, nal(0x09, 5)...)
	stream = append(stream, nal(0x41, 30)...) // non-IDR (type 1)

	va.PushPES(stream, 100, 9000)
	va.PushPES(nal(0x41, 20), 200, 12000)
	va.Flush()
	units := va.Units()
	if len(units) != 3 {
		t.Fatalf("units %d: %+v", len(units), units)
	}
	if !units[0].Keyframe || units[1].Keyframe || units[2].Keyframe {
		t.Fatalf("keyframes: %v %v %v", units[0].Keyframe, units[1].Keyframe, units[2].Keyframe)
	}
	if units[0].SliceCount != 1 || units[1].SliceCount != 1 {
		t.Fatalf("slices: %d %d", units[0].SliceCount, units[1].SliceCount)
	}
	if units[0].ByteOffset != 100 {
		t.Fatalf("offset %d", units[0].ByteOffset)
	}
	if units[0].Ticks != 9000 || units[1].Ticks != 9000 || units[2].Ticks != 12000 {
		t.Fatalf("ticks %d %d %d", units[0].Ticks, units[1].Ticks, units[2].Ticks)
	}
}

func TestVideoAssemblerStreamsAcrossPushes(t *testing.T) {
	va := NewVideoAssembler(StreamTypeH264)
	// Split a unit across pushes mid-NAL, as packetisation would.
	unit := append(nal(0x09, 5), nal(0x65, 60)...)
	va.PushPES(unit[:30], 0, 1000)
	va.PushPES(unit[30:], 0, 1000)
	va.PushPES(nal(0x41, 30), 0, 4000)
	va.Flush()
	if got := len(va.Units()); got != 2 {
		t.Fatalf("units %d", got)
	}
	if !va.Units()[0].Keyframe {
		t.Fatal("first should be keyframe")
	}
}

func TestKeyframeIntervalsP95(t *testing.T) {
	units := []AccessUnit{
		{Keyframe: true, Ticks: 90000},
		{Keyframe: true, Ticks: 270000},
		{Keyframe: true, Ticks: 450000},
		{Keyframe: true, Ticks: 630000},
	}
	iv := KeyframeIntervals(units)
	if len(iv) != 3 {
		t.Fatalf("intervals %d", len(iv))
	}
	for _, got := range iv {
		if got != 180000 {
			t.Fatalf("interval %d", got)
		}
	}
}

func TestStartCodeLength(t *testing.T) {
	if got := startCodeLen([]byte{0, 0, 1}); got != 3 {
		t.Fatalf("3-byte start code: %d", got)
	}
	if got := startCodeLen([]byte{0, 0, 0, 1}); got != 4 {
		t.Fatalf("4-byte start code: %d", got)
	}
	if findStartCode([]byte{9, 9, 0, 0, 1, 9}, 0) != 2 {
		t.Fatal("find index")
	}
	if findStartCode([]byte{9, 9, 9}, 0) != -1 {
		t.Fatal("no start code")
	}
}
