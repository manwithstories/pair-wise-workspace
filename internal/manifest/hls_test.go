package manifest

import (
	"errors"
	"strings"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
)

func TestParseHLSBasic(t *testing.T) {
	playlist := []byte(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
seg100.m4s
#EXT-X-DISCONTINUITY
#EXTINF:3.5,
seg101.m4s
#EXTINF:4.0,
seg102.m4s
`)
	evs, err := ParseHLS(playlist, 0, Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4", len(evs))
	}
	if evs[0].Kind != KindInit || evs[0].URI != "init.mp4" {
		t.Fatalf("init event wrong: %+v", evs[0])
	}
	first := evs[1]
	if first.Sequence != 100 || first.URI != "seg100.m4s" {
		t.Fatalf("first media = %+v", first)
	}
	if first.Duration != clock.Ticks(4*clock.Rate) || first.Start != 0 {
		t.Fatalf("first timing = %+v", first)
	}
	second := evs[2]
	if !second.Discontinuity {
		t.Fatal("discontinuity flag missing")
	}
	if second.Start != clock.Ticks(4*clock.Rate) || second.Duration != clock.Ticks(3500*90) {
		t.Fatalf("second timing = start %d dur %d", second.Start, second.Duration)
	}
	if evs[3].Sequence != 102 {
		t.Fatalf("third seq = %d", evs[3].Sequence)
	}
	for _, e := range evs {
		if e.Protocol != ProtocolHLS || e.TrackID != 0 {
			t.Fatalf("metadata wrong: %+v", e)
		}
	}
}

func TestParseHLSByteRange(t *testing.T) {
	playlist := []byte(`#EXTM3U
#EXT-X-BYTERANGE:1000@0
#EXTINF:2.0,
frag.bin
#EXT-X-BYTERANGE:1500
#EXTINF:2.0,
frag.bin
`)
	evs, err := ParseHLS(playlist, 1, Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if evs[0].ByteRange == nil || evs[0].ByteRange.Start != 0 || evs[0].ByteRange.Length != 1000 {
		t.Fatalf("first range wrong: %+v", evs[0].ByteRange)
	}
	if evs[1].ByteRange.Start != 1000 || evs[1].ByteRange.Length != 1500 {
		t.Fatalf("continuation range wrong: %+v", evs[1].ByteRange)
	}
}

func TestParseHLSRejects(t *testing.T) {
	base := func(lines ...string) []byte {
		return []byte(strings.Join(append([]string{"#EXTM3U"}, lines...), "\n"))
	}
	cases := map[string][]byte{
		"no header":              []byte("#EXTINF:1,\na.ts\n"),
		"path traversal":         base("#EXTINF:1,", "../escape.ts"),
		"absolute path":          base("#EXTINF:1,", "/etc/passwd"),
		"remote uri":             base("#EXTINF:1,", "https://host/a.ts"),
		"backslash":              base("#EXTINF:1,", `dir\a.ts`),
		"bad extinf":             base("#EXTINF:NaN,", "a.ts"),
		"dangling extinf":        base("#EXTINF:1,"),
		"encrypted":              base("#EXT-X-KEY:METHOD=AES-128,URI=k.bin", "#EXTINF:1,", "a.ts"),
		"bad media sequence":     base("#EXT-X-MEDIA-SEQUENCE:-1"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHLS(data, 0, Options{}); err == nil {
				t.Fatalf("%s: expected error", name)
			}
		})
	}
}

func TestSequenceConflictOnMerge(t *testing.T) {
	// Two playlists delivered by different capture nodes with overlapping
	// MEDIA-SEQUENCE ranges: each parses on its own, the merge must fail.
	first := []byte("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\na.ts\n#EXTINF:1.0,\nb.ts\n")
	second := []byte("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:1.0,\nb2.ts\n#EXTINF:1.0,\nc.ts\n")
	e1, err := ParseHLS(first, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := ParseHLS(second, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSequenceConflicts(append(e1, e2...)); !errors.Is(err, clock.ErrDuplicateSeq) {
		t.Fatalf("want ErrDuplicateSeq, got %v", err)
	}
	// Re-delivery of the same URI at the same sequence is idempotent.
	if err := CheckSequenceConflicts(append(e1, e1...)); err != nil {
		t.Fatalf("identical re-delivery should be accepted: %v", err)
	}
}

func TestParseHLSSizeLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < 5; i++ {
		b.WriteString("#EXTINF:1.0,\nseg.ts\n")
	}
	_, err := ParseHLS([]byte(b.String()), 0, Options{MaxSegments: 3})
	if !errors.Is(err, clock.ErrManifestTooLarge) {
		t.Fatalf("want ErrManifestTooLarge, got %v", err)
	}
}

func TestParseHLSTrackLimit(t *testing.T) {
	_, err := ParseHLS([]byte("#EXTM3U\n"), 99, Options{MaxTracks: 4})
	if !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}
