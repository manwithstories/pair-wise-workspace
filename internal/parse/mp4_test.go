package parse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"mediarun/internal/model"
)

// buildSampleMP4 assembles a small valid movie used across parse tests.
func buildSampleMP4(t *testing.T) []byte {
	t.Helper()
	m := parseFixtureMovie(t)
	_ = m
	if false {
		m.Tracks = []Track{
			{
				ID:        1,
				Handler:   "vide",
				Timescale: 90000,
				Width:     64,
				Height:    36,
				Language:  "und",
				Entry:     RawBox{Type: [4]byte{'a', 'v', 'c', '1'}, Size: 8, HeaderSize: 8},
				Samples: []Sample{
					{Index: 1, Duration: 3000, DTS: 0, Sync: true, Data: mustSEI(t)},
					{Index: 2, Duration: 3000, DTS: 3000, Sync: false, Data: mustSEI(t)},
					{Index: 3, Duration: 3000, DTS: 6000, Sync: true, Data: mustSEI(t)},
				},
				Keyframes: []uint32{1, 3},
			},
			{
				ID:        2,
				Handler:   "soun",
				Timescale: 48000,
				Language:  "und",
				Entry:     RawBox{Type: [4]byte{'m', 'p', '4', 'a'}, Size: 8, HeaderSize: 8},
				Samples: []Sample{
					{Index: 1, Duration: 48000, DTS: 0, Sync: true, Data: mustID3(t)},
				},
			},
		}
	}
	data, err := EncodeMovie(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

func parseFixtureMovie(t *testing.T) *Movie {
	t.Helper()
	m := &Movie{Timescale: 1000}
	m.Tracks = []Track{
		{
			ID:        1,
			Handler:   "vide",
			Timescale: 90000,
			Width:     64,
			Height:    36,
			Language:  "und",
			Entry:     RawBox{Type: [4]byte{'a', 'v', 'c', '1'}, Size: 8, HeaderSize: 8},
			Samples: []Sample{
				{Index: 1, Duration: 3000, DTS: 0, Sync: true, Data: mustSEI(t)},
				{Index: 2, Duration: 3000, DTS: 3000, Sync: false, Data: mustSEI(t)},
				{Index: 3, Duration: 3000, DTS: 6000, Sync: true, Data: mustSEI(t)},
			},
			Keyframes: []uint32{1, 3},
		},
		{
			ID:        2,
			Handler:   "soun",
			Timescale: 48000,
			Language:  "und",
			Entry:     RawBox{Type: [4]byte{'m', 'p', '4', 'a'}, Size: 8, HeaderSize: 8},
			Samples: []Sample{
				{Index: 1, Duration: 48000, DTS: 0, Sync: true, Data: mustID3(t)},
			},
		},
	}
	m.Udta = []RawBox{{
		Type:       [4]byte{'c', 'h', 'p', 'l'},
		Size:       uint64(8 + len(mustChpl(t))),
		HeaderSize: 8,
		Payload:    mustChpl(t),
	}}
	return m
}

func mustSEI(t *testing.T) []byte {
	t.Helper()
	data, err := EncodeSEI([16]byte{1, 2, 3}, []byte("cue-1"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustID3(t *testing.T) []byte {
	t.Helper()
	data, err := EncodeID3(&ID3Tag{ID: "audio-1", Text: "take-1", Owner: "stage", Private: []byte{9}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustChpl(t *testing.T) []byte {
	t.Helper()
	data, err := encodeChpl([]Chapter{
		{ID: "chapter-1", Start: 90000, Title: "Overture"},
		{ID: "chapter-2", Start: 360000, Title: "Finale"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEncodeParseRoundTrip(t *testing.T) {
	data := buildSampleMP4(t)
	m, err := ParseMovie(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Tracks) != 2 {
		t.Fatalf("tracks: %d", len(m.Tracks))
	}
	video := m.Tracks[0]
	if video.Handler != "vide" || video.Timescale != 90000 || video.Width != 64 {
		t.Fatalf("video track: %+v", video)
	}
	if len(video.Samples) != 3 {
		t.Fatalf("samples: %d", len(video.Samples))
	}
	if video.Samples[1].DTS != 3000 || !video.Samples[0].Sync || video.Samples[1].Sync {
		t.Fatalf("sample table: %+v", video.Samples)
	}
	if len(video.Keyframes) != 2 || video.Keyframes[0] != 1 || video.Keyframes[1] != 3 {
		t.Fatalf("keyframes: %v", video.Keyframes)
	}
	if string(video.Samples[0].Data) != string(mustSEI(t)) {
		t.Fatal("video sample bytes changed")
	}

	// Re-encode and reparsing must be identical.
	again, err := EncodeMovie(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	m2, err := ParseMovie(again)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got, want := digestMovie(t, m2), digestMovie(t, m); got != want {
		t.Fatalf("roundtrip digest %s != %s", got, want)
	}
	if !bytes.Equal(again, data) {
		t.Fatal("deterministic encode expected")
	}
}

func digestMovie(t *testing.T, m *Movie) string {
	t.Helper()
	meta, err := ExtractMetadata(m)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	frames, err := FrameEvents(m)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, ev := range meta.Events() {
		h.Write([]byte(ev.Digest()))
	}
	for _, ev := range frames {
		h.Write([]byte(ev.Digest()))
	}
	for _, track := range m.Tracks {
		h.Write([]byte(track.Entry.DigestString()))
		for _, k := range track.Keyframes {
			h.Write([]byte{byte(k)})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DigestString is a test-facing helper mirroring RawBox.Digest.
func (b RawBox) DigestString() string {
	d := b.Digest()
	return hex.EncodeToString(d[:])
}

func TestMetadataExtraction(t *testing.T) {
	m, err := ParseMovie(buildSampleMP4(t))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ExtractMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.SEI) != 3 {
		t.Fatalf("sei: %d", len(meta.SEI))
	}
	if meta.SEI[1].Start != 3000 {
		t.Fatalf("sei timing: %d", meta.SEI[1].Start)
	}
	if len(meta.ID3) != 1 || meta.ID3[0].Tag.ID != "audio-1" || meta.ID3[0].Tag.Text != "take-1" {
		t.Fatalf("id3: %+v", meta.ID3)
	}
	if len(meta.Chapters) != 2 || meta.Chapters[1].Title != "Finale" {
		t.Fatalf("chapters: %+v", meta.Chapters)
	}
	if meta.Chapters[0].Start != 90000 {
		t.Fatalf("chapter tick: %d", meta.Chapters[0].Start)
	}
	events := meta.Events()
	if len(events) != 6 {
		t.Fatalf("events: %d", len(events))
	}
	model.SortStable(events)
	for i := 1; i < len(events); i++ {
		if events[i].Start < events[i-1].Start {
			t.Fatalf("unsorted events at %d", i)
		}
	}
}

func TestTruncatedInputRejected(t *testing.T) {
	data := buildSampleMP4(t)
	for _, cut := range []int{0, 4, 8, len(data) / 2, len(data) - 1} {
		part := data[:cut]
		if _, err := ParseMovie(part); err == nil {
			t.Fatalf("truncated at %d accepted", cut)
		} else if model.ClassOf(err) != model.ClassCorrupt {
			t.Fatalf("truncated at %d: class %v", cut, model.ClassOf(err))
		}
	}
}

func TestBadBoxLengthRejected(t *testing.T) {
	data := buildSampleMP4(t)
	bad := append([]byte(nil), data...)
	// Overwrite the ftyp size with a 4-byte value that is impossible.
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 0, 3
	if _, err := ParseMovie(bad); !errors.Is(err, model.ErrBadLength) {
		t.Fatalf("bad size: %v", err)
	}
	// A box larger than the remaining file.
	bad = append([]byte(nil), data...)
	bad[0], bad[1], bad[2], bad[3] = 0x7F, 0xFF, 0xFF, 0xFF
	if _, err := ParseMovie(bad); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestUnknownBoxPreserved(t *testing.T) {
	m := &Movie{Timescale: 1000}
	m.Tracks = []Track{{
		ID: 1, Handler: "vide", Timescale: 1000, Language: "und",
		Entry:   RawBox{Type: [4]byte{'a', 'v', 'c', '1'}, Size: 8, HeaderSize: 8},
		Samples: []Sample{{Index: 1, Duration: 1000, DTS: 0, Sync: true, Data: []byte{1, 2, 3}}},
	}}
	odd := RawBox{
		Type:       [4]byte{'z', 'z', 'z', 'z'},
		Size:       8 + 5,
		HeaderSize: 8,
		Payload:    []byte{0xde, 0xad, 0xbe, 0xef, 0x00},
	}
	m.Extras = []RawBox{odd}
	data, err := EncodeMovie(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMovie(data)
	if err != nil {
		t.Fatal(err)
	}
	var found *RawBox
	for i := range parsed.Extras {
		if parsed.Extras[i].String() == "zzzz" {
			found = &parsed.Extras[i]
		}
	}
	if found == nil {
		t.Fatal("unknown box dropped")
	}
	if !bytes.Equal(found.Payload, odd.Payload) {
		t.Fatalf("unknown payload changed: %x", found.Payload)
	}
	if found.Digest() != odd.Digest() {
		t.Fatal("unknown box digest changed")
	}
	out, err := EncodeMovie(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("re-encode with unknown box differs")
	}
}
