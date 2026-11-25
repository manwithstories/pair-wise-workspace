package mapping

import (
	"testing"

	"mediqc/internal/media"
)

func pmtStreams() []struct {
	PID   uint16
	Codec media.Codec
	Kind  media.TrackKind
} {
	return []struct {
		PID   uint16
		Codec media.Codec
		Kind  media.TrackKind
	}{
		{0x100, media.CodecH264, media.TrackVideo},
		{0x101, media.CodecAAC, media.TrackAudio},
	}
}

func TestParseJSONAndValidate(t *testing.T) {
	data := []byte(`{"routes":[
		{"source_pid":256,"track_id":1,"kind":"video","codec":"h264","target":"video"},
		{"source_pid":257,"track_id":2,"kind":"audio","codec":"aac-lc","target":"audio"}
	]}`)
	m, err := ParseJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	tracks, verr := m.ValidateAgainstPMT(pmtStreams())
	if verr != nil {
		t.Fatal(verr)
	}
	if len(tracks) != 2 || tracks[0].PID != 256 {
		t.Fatalf("tracks=%+v", tracks)
	}
}

func TestParseSpecRejectsDuplicateTarget(t *testing.T) {
	_, err := ParseSpec("0x100:1:video:h264:out,0x101:2:audio:aac-lc:out")
	if err == nil || err.Code != media.CodeMappingInvalid {
		t.Fatalf("expected duplicate target error, got %v", err)
	}
}

func TestValidationRejectsCodecMismatchWithoutDefault(t *testing.T) {
	m := &Mapping{Routes: []Route{{SourcePID: 0x100, TrackID: 1, Kind: media.TrackVideo, Codec: media.CodecH264, Target: "v"}}}
	streams := pmtStreams()
	streams[0].Codec = media.CodecAAC
	if _, err := m.ValidateAgainstPMT(streams); err == nil || err.Code != media.CodeMappingInvalid {
		t.Fatalf("expected codec mismatch, got %v", err)
	}

	missing := &Mapping{Routes: []Route{{SourcePID: 0x777, TrackID: 9, Kind: media.TrackVideo, Codec: media.CodecH264, Target: "x"}}}
	if _, err := missing.ValidateAgainstPMT(pmtStreams()); err == nil || err.Code != media.CodeMappingInvalid {
		t.Fatalf("expected absent PID refusal, got %v", err)
	}
}

func TestUnsupportedCodecRejected(t *testing.T) {
	_, err := ParseSpec("0x102:3:audio:mp3:audio")
	if err == nil || err.Code != media.CodeUnsupported {
		t.Fatalf("expected unsupported codec, got %v", err)
	}
}
