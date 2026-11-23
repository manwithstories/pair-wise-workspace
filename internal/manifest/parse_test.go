package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archive.local/archive/internal/model"
)

func parseString(t *testing.T, body string) (*Manifest, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return ParseFile(path)
}

const goodManifest = `
# archive material list
track 1 reels/a.mp4 90000 324000000 video 648000000
track 2 reels/b.mp4 48000 172800000 audio 345600000
edit 1 324000000 162000000 1 1
edit 2 -1 48000 1 1
edit 2 172800000 172752000 1 1
digest 1 ` + digestA + `
`

const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParseGoodManifest(t *testing.T) {
	m, err := parseString(t, goodManifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("entries = %d", len(m.Entries))
	}
	if m.Entries[0].TrackID != 1 || m.Entries[0].Timescale != 90000 {
		t.Fatalf("track 1 = %+v", m.Entries[0])
	}
	if m.Entries[0].OriginTicks != 324000000 || m.Entries[0].Duration != 648000000 {
		t.Fatalf("origin/duration = %d/%d", m.Entries[0].OriginTicks, m.Entries[0].Duration)
	}
	if m.Entries[0].Digest != digestA {
		t.Fatalf("digest = %q", m.Entries[0].Digest)
	}
	if len(m.Entries[1].Edit) != 2 {
		t.Fatalf("audio edits = %d", len(m.Entries[1].Edit))
	}
	if m.Entries[1].Edit[0].MediaTime != -1 {
		t.Fatalf("empty edit media time = %d", m.Entries[1].Edit[0].MediaTime)
	}
	tl, err := m.Timeline()
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if u, _ := tl.Unified(1, 324000000); u != 3600*1_000_000 {
		t.Fatalf("video origin unified = %d", u)
	}
	if u, _ := tl.Unified(2, 172800000); u != 3600*1_000_000 {
		t.Fatalf("audio origin unified = %d", u)
	}
}

func TestParseNamesLineAndTrackForBadReference(t *testing.T) {
	cases := []struct {
		body    string
		line    int
		track   uint32
		code    model.Code
		details string
	}{
		{"track 1 a.mp4 90000 0 video\ntrack 2 b.mp4 48000 0 audio\nedit 3 0 100 1 1\n", 3, 3, model.CodeManifestReference, "undeclared track"},
		{"track 1 a.mp4 90000 0 video\ntrack 2 b.mp4 48000 0 audio\nedit 2 0 100 1 0\n", 3, 0, model.CodeManifestSyntax, "rate denominator"},
		{"track 1 a.mp4 90000 0 video\ntrack 1 b.mp4 48000 0 audio\n", 2, 1, model.CodeManifestReference, "duplicate track id"},
		{"track 1 a.mp4 90000 0 video\ndigest 2 " + digestA + "\n", 2, 2, model.CodeManifestReference, "undeclared track"},
		{"track 1 a.mp4 90000 0 video\ntrack 2 b.mp4 48000 0 audio\ntrack 2 c.mp4 48000 0 audio\n", 3, 2, model.CodeManifestReference, "duplicate track id"},
		{"track 1 ../escape.mp4 90000 0 video\n", 1, 1, model.CodeManifestReference, "escapes"},
		{"track 1 a.wav 90000 0 video\n", 1, 1, model.CodeManifestReference, ".mp4"},
	}
	for _, c := range cases {
		_, err := parseString(t, c.body)
		if err == nil {
			t.Fatalf("expected error for %q", c.body)
		}
		if model.CodeOf(err) != c.code {
			t.Fatalf("code = %q want %q (%v)", model.CodeOf(err), c.code, err)
		}
		var me *model.Error
		if !asModel(err, &me) {
			t.Fatalf("not a model error: %v", err)
		}
		if me.Line != c.line {
			t.Errorf("line = %d want %d (%v)", me.Line, c.line, err)
		}
		if me.Track != c.track && c.code == model.CodeManifestReference {
			t.Errorf("track = %d want %d (%v)", me.Track, c.track, err)
		}
		if !strings.Contains(me.Detail, c.details) {
			t.Errorf("detail = %q want substring %q", me.Detail, c.details)
		}
	}
}

func TestEditListOutOfRangeNamesLineAndTrack(t *testing.T) {
	body := "track 1 a.mp4 90000 0 video 9000\n" +
		"track 2 b.mp4 48000 0 audio 4800\n" +
		"edit 1 0 9000 1 1\n" +
		"edit 2 0 99999 1 1\n"
	_, err := parseString(t, body)
	if model.CodeOf(err) != model.CodeEditListRange {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
	var me *model.Error
	asModel(err, &me)
	if me.Line != 4 || me.Track != 2 {
		t.Fatalf("line/track = %d/%d", me.Line, me.Track)
	}
}

func TestImplicitEditFromOrigin(t *testing.T) {
	m, err := parseString(t, "track 5 x.mp4 90000 450000 video\n")
	if err != nil {
		t.Fatal(err)
	}
	e := m.Entries[0]
	if len(e.Edit) != 1 || e.Edit[0].MediaTime != 450000 || e.Edit[0].SegmentDuration != -1 {
		t.Fatalf("implicit edit = %+v", e.Edit)
	}
}

func TestOriginPlusDurationOverflow(t *testing.T) {
	body := "track 1 a.mp4 90000 9223372036854775000 video 900\n"
	_, err := parseString(t, body)
	if model.CodeOf(err) != model.CodeTimescaleOverflow {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
}

func TestParseRejectsEmptyManifest(t *testing.T) {
	_, err := parseString(t, "# nothing here\n\n")
	if model.CodeOf(err) != model.CodeManifestSyntax {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
}

func TestResolveSourceStaysInDir(t *testing.T) {
	m := &Manifest{Dir: "/tmp/work"}
	if _, err := m.resolve(1, 1, "reels/a.mp4"); err != nil {
		t.Fatalf("nested source rejected: %v", err)
	}
	if _, err := m.resolve(1, 1, "/etc/passwd.mp4"); model.CodeOf(err) != model.CodeManifestReference {
		t.Fatalf("absolute escape accepted: %v", err)
	}
}

func asModel(err error, target **model.Error) bool {
	me, ok := err.(*model.Error)
	if ok {
		*target = me
	}
	return ok
}
