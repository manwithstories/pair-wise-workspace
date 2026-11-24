package mkv

import (
	"bytes"
	"errors"
	"testing"

	"review/internal/ebml"
	"review/internal/fixture"
	"review/internal/media"
)

func decodeBytes(t *testing.T, raw []byte) *Document {
	t.Helper()
	doc, err := Decode(ebml.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc
}

func TestDecodeStudioSample(t *testing.T) {
	doc := decodeBytes(t, fixture.StudioInterview(42).Build())
	if doc.TimecodeScale != 1_000_000 {
		t.Fatalf("timecode scale = %d", doc.TimecodeScale)
	}
	if len(doc.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(doc.Tracks))
	}
	// Track metadata survives the round trip, including colour fields.
	var video, audio *Track
	for i := range doc.Tracks {
		switch doc.Tracks[i].Type {
		case fixture.TrackTypeVideo:
			video = &doc.Tracks[i]
		case fixture.TrackTypeAudio:
			audio = &doc.Tracks[i]
		}
	}
	if video == nil || video.Matrix == 0 || video.Transfer == 0 {
		t.Fatalf("video colour metadata lost: %+v", video)
	}
	if audio == nil || audio.Channels != 2 || audio.SampleRate != 48000 {
		t.Fatalf("audio metadata wrong: %+v", audio)
	}
	if len(doc.Blocks) == 0 {
		t.Fatal("no blocks decoded")
	}
	// All timestamps must be on the nanosecond base and non-negative.
	var keyframes int
	first := doc.Blocks[0].Time
	for _, b := range doc.Blocks {
		if b.Time.Nanoseconds()%media.NanosPerMilli != 0 {
			t.Fatalf("block time %d not on millisecond grid", b.Time)
		}
		if b.Time < 0 {
			t.Fatalf("negative block time %d", b.Time)
		}
		if b.Offset < 0 || len(b.Payload) == 0 {
			t.Fatalf("block provenance lost: %+v", b)
		}
		if b.Keyframe {
			keyframes++
		}
	}
	if first != 0 {
		t.Fatalf("first block time = %v, want 0", first)
	}
	if keyframes == 0 {
		t.Fatal("no keyframes decoded")
	}
}

func TestDecodeUnknownLayoutAndCluster(t *testing.T) {
	d := fixture.RemoteNarration(11)
	d.UnknownSegment = true
	d.UnknownCluster = true
	doc := decodeBytes(t, d.Build())
	var narration *Track
	for i := range doc.Tracks {
		if doc.Tracks[i].Number == 1 {
			narration = &doc.Tracks[i]
		}
	}
	if narration == nil || narration.Channels != 0 {
		t.Fatalf("unknown channel layout must decode as 0/absent: %+v", narration)
	}
	if len(doc.Blocks) == 0 {
		t.Fatal("blocks inside unknown-size cluster were lost")
	}
}

func TestDecodePreservesInterleavingOrder(t *testing.T) {
	doc := decodeBytes(t, fixture.LocationBRoll(5).Build())
	for i := 1; i < len(doc.Blocks); i++ {
		a, b := doc.Blocks[i-1], doc.Blocks[i]
		if b.Time < a.Time || (b.Time == a.Time && b.Track < a.Track) {
			t.Fatalf("interleaving order violated at %d: %+v then %+v", i, a, b)
		}
	}
}

func TestDecodeCorruptionsPropagate(t *testing.T) {
	t.Run("truncation carries first missing offset", func(t *testing.T) {
		raw := fixture.TruncateTail(fixture.LocationBRoll(42))
		_, err := Decode(ebml.NewReader(bytes.NewReader(raw)))
		if err == nil {
			t.Fatal("truncated stream decoded without error")
		}
		var te *ebml.TruncatedError
		if !errors.As(err, &te) {
			t.Fatalf("want TruncatedError, got %v", err)
		}
		if te.MissingOffset != int64(len(raw)) {
			t.Fatalf("missing offset = %d, want %d", te.MissingOffset, len(raw))
		}
		var de *DecodeError
		if !errors.As(err, &de) {
			t.Fatal("parser error must be wrapped in DecodeError")
		}
	})
	t.Run("out-of-range block is a boundary error", func(t *testing.T) {
		raw := fixture.OutOfRangeBlock(fixture.StudioInterview(42))
		_, err := Decode(ebml.NewReader(bytes.NewReader(raw)))
		var be *ebml.BoundaryError
		if !errors.As(err, &be) {
			t.Fatalf("want BoundaryError, got %v", err)
		}
	})
}

func TestValueDecoders(t *testing.T) {
	if v, err := DecodeUint([]byte{0x01, 0x02}); err != nil || v != 0x0102 {
		t.Fatalf("uint decode: %d %v", v, err)
	}
	if _, err := DecodeUint(nil); err == nil {
		t.Fatal("empty uint must error")
	}
	if f, err := DecodeFloat(nil); err != nil || f != 0 {
		t.Fatalf("zero-length float: %f %v", f, err)
	}
	if _, err := DecodeFloat([]byte{1, 2, 3}); err == nil {
		t.Fatal("3-byte float must error")
	}
	if _, _, _, _, err := DecodeBlock([]byte{0x01, 0x00}); err == nil {
		t.Fatal("short block must error")
	}
}
