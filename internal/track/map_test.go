package track

import (
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
)

func video(n uint64, codec string) Source {
	return Source{Number: n, Type: TypeVideo, CodecID: codec, PixelWidth: 640, PixelHeight: 360}
}

func audio(n uint64, codec string) Source {
	return Source{Number: n, Type: TypeAudio, CodecID: codec, SamplingFrequency: 48000, Channels: 2}
}

func TestMapSupportedCodecs(t *testing.T) {
	src := []Source{
		video(1, "V_MPEG4/ISO/AVC"),
		audio(2, "A_AAC/MPEG4/LC"),
		{Number: 3, Type: TypeSubtitle, CodecID: "S_TEXT/UTF8"},
		audio(4, "A_OPUS"),
		video(5, "V_VP9"),
	}
	tracks, unknown, err := Map(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 || len(tracks) != 5 {
		t.Fatalf("tracks=%d unknown=%d", len(tracks), len(unknown))
	}
	if tracks[0].DestTrackID != 1 || tracks[4].DestTrackID != 5 {
		t.Fatalf("dest IDs not assigned in order: %+v", tracks)
	}
	if tracks[1].Mapping.Entry != "mp4a" || tracks[1].ConfigMode != ConfigESDS {
		t.Fatalf("AAC mapping wrong: %+v", tracks[1])
	}
}

func TestMapDuplicateTrackNumber(t *testing.T) {
	_, _, err := Map([]Source{video(1, "V_VP9"), audio(1, "A_OPUS")})
	if !errors.Is(err, fault.ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestMapZeroTrackNumber(t *testing.T) {
	_, _, err := Map([]Source{video(0, "V_VP9")})
	if !errors.Is(err, fault.ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestMapUnknownCodecCounted(t *testing.T) {
	tracks, unknown, err := Map([]Source{
		video(1, "V_MPEG2"),
		audio(2, "A_AC3"),
		video(3, "V_VP9"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || len(unknown) != 2 {
		t.Fatalf("tracks=%d unknown=%d", len(tracks), len(unknown))
	}
	r := NewReport(tracks, unknown)
	if r.UnknownTracks != 2 || len(r.UnknownCodecIDs) != 2 {
		t.Fatalf("report=%+v", r)
	}
}

func TestMapVideoOnAudioTrackRejected(t *testing.T) {
	src := []Source{{Number: 1, Type: TypeAudio, CodecID: "V_VP9", SamplingFrequency: 8000, Channels: 1}}
	tracks, unknown, err := Map(src)
	if err != nil || len(tracks) != 0 || len(unknown) != 1 {
		t.Fatalf("tracks=%d unknown=%d err=%v", len(tracks), len(unknown), err)
	}
}

func TestMapAudioMissingRateFails(t *testing.T) {
	src := []Source{{Number: 1, Type: TypeAudio, CodecID: "A_OPUS"}}
	_, _, err := Map(src)
	if !errors.Is(err, fault.ErrMalformed) {
		t.Fatalf("got %v", err)
	}
	if fault.TrackOf(err) != 1 {
		t.Fatalf("track=%d", fault.TrackOf(err))
	}
}

func TestMapVideoZeroDimensionsFails(t *testing.T) {
	src := []Source{{Number: 1, Type: TypeVideo, CodecID: "V_MPEG4/ISO/AVC"}}
	_, _, err := Map(src)
	if !errors.Is(err, fault.ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestCodecForSuffix(t *testing.T) {
	if m, ok := CodecFor("A_AAC/MPEG4/LC"); !ok || m.Kind != KindAAC {
		t.Fatalf("suffix codec not resolved: %+v %v", m, ok)
	}
	if _, ok := CodecFor("A_AC3"); ok {
		t.Fatal("AC-3 must stay unsupported until a mapping is documented")
	}
}
