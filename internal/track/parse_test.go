package track

import (
	"bytes"
	"testing"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
)

func u(id uint32, v uint64) []byte {
	return ebml.EncodeElement(id, ebml.EncodeUnsigned(v))
}

func f(id uint32, v float64) []byte {
	return ebml.EncodeElement(id, ebml.EncodeFloat(v))
}

func s(id uint32, v string) []byte {
	return ebml.EncodeElement(id, []byte(v))
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestParseTracksAudioVideo(t *testing.T) {
	audio := append(u(model.IDChannels, 6),
		f(model.IDSamplingFreq, 48000)...)
	audio = append(audio, u(model.IDBitDepth, 24)...)
	vEntry := cat(
		u(model.IDTrackNumber, 1), u(model.IDTrackUID, 101),
		u(model.IDTrackType, 1), s(model.IDCodecID, "V_VP9"),
	)
	video := cat(u(model.IDPixelWidth, 1920), u(model.IDPixelHeight, 1080))
	// Colour present but missing three fields (only matrix supplied).
	colour := u(model.IDColorMatrix, 9)
	video = append(video, ebml.EncodeElement(model.IDColour, colour)...)
	vEntry = append(vEntry, ebml.EncodeElement(model.IDVideo, video)...)

	aEntry := cat(
		u(model.IDTrackNumber, 2), u(model.IDTrackUID, 202),
		u(model.IDTrackType, 2), s(model.IDCodecID, "A_OPUS"),
		ebml.EncodeElement(model.IDAudio, audio),
	)
	tracks := append(
		ebml.EncodeElement(model.IDTrackEntry, vEntry),
		ebml.EncodeElement(model.IDTrackEntry, aEntry)...,
	)

	got, err := ParseTracks(tracks)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("track count: %d", len(got))
	}
	v := got[0]
	if v.Kind != model.KindVideo || v.Video.PixelWidth != 1920 || !v.Video.ColorPresent {
		t.Fatalf("video track wrong: %+v", v)
	}
	if len(v.Video.MissingColorFields) != 3 {
		t.Fatalf("missing color fields not reported: %+v", v.Video.MissingColorFields)
	}
	a := got[1]
	if a.Kind != model.KindAudio || a.Audio.Channels != 6 ||
		a.Audio.SamplingFrequency != 48000 || a.Audio.BitDepth != 24 {
		t.Fatalf("audio track wrong: %+v", a.Audio)
	}
}

func TestParseTracksMissingUIDAnchored(t *testing.T) {
	entry := u(model.IDTrackNumber, 1)
	entry = append(entry, u(model.IDTrackType, 1)...)
	tracks := ebml.EncodeElement(model.IDTrackEntry, entry)
	_, err := ParseTracks(tracks)
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "TRACK_MISSING_UID" {
		t.Fatalf("want anchored missing UID, got %v", err)
	}
	if ae.Offset < 0 {
		t.Fatal("error must carry the TrackEntry source offset")
	}
}

func TestParseTracksUnsupportedType(t *testing.T) {
	entry := append(u(model.IDTrackUID, 9), u(model.IDTrackType, 42)...)
	tracks := ebml.EncodeElement(model.IDTrackEntry, entry)
	if _, err := ParseTracks(tracks); err == nil {
		t.Fatal("unsupported track type must be rejected")
	}
}

func TestParseTrackOperationNested(t *testing.T) {
	joinBlocks := ebml.EncodeElement(model.IDTrackJoinBlocks,
		u(model.IDTrackJoinUID, 42))
	operation := ebml.EncodeElement(model.IDTrackOperation, joinBlocks)
	entry := cat(
		u(model.IDTrackUID, 7), u(model.IDTrackNumber, 1),
		u(model.IDTrackType, 1), s(model.IDCodecID, "V_VP9"),
		operation,
	)
	tracks := ebml.EncodeElement(model.IDTrackEntry, entry)
	got, err := ParseTracks(tracks)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].JoinUID != 42 {
		t.Fatalf("nested TrackJoinUID not resolved: %d", got[0].JoinUID)
	}
}

func TestRoundTripFixtureIsStable(t *testing.T) {
	// Sanity: the test helper encoding must be re-readable by the EBML reader.
	raw := u(model.IDTrackNumber, 7)
	r := ebml.NewReader(bytes.NewReader(raw))
	e, err := r.Next()
	if err != nil || e.ID != model.IDTrackNumber || e.Size != 1 {
		t.Fatalf("fixture encoding broken: %+v %v", e, err)
	}
}
