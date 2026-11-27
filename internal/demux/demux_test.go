package demux

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avpipe/internal/fixture"
	"avpipe/internal/media"
)

// fixtureSet generates the catalog once per test binary run.
func fixtureSet(t *testing.T) fixture.Set {
	t.Helper()
	dir, err := os.MkdirTemp("", "avpipe-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	set, err := fixture.GenerateAll(dir, 17)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func findFixture(t *testing.T, set fixture.Set, name string) fixture.File {
	t.Helper()
	for _, f := range set.Files {
		if filepath.Base(f.Path) == name {
			return f
		}
	}
	t.Fatalf("fixture %q not found", name)
	return fixture.File{}
}

func TestOpenGoodMP4(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer in.Close()
	if in.Container != ContainerMP4 {
		t.Fatalf("container = %q", in.Container)
	}
	if len(in.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(in.Tracks))
	}
	for _, tr := range in.Tracks {
		if tr.TimeBase.String() != "1/15360" && tr.Kind == media.TrackVideo {
			t.Logf("video time base %s", tr.TimeBase)
		}
	}
	if _, ok := in.Track("v0"); !ok {
		t.Fatal("video track v0 missing")
	}
	if _, ok := in.Track("a1"); !ok {
		t.Fatal("audio track a1 missing")
	}
}

func TestOpenGoodMXF(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mxf")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer in.Close()
	if in.Container != ContainerMXF {
		t.Fatalf("container = %q", in.Container)
	}
	if len(in.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(in.Tracks))
	}
}

func TestTruncatedMXFRejectsCorruptPackets(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "trunc.mxf")
	in, err := Open(f.Path, Options{})
	if err != nil {
		// Some truncation levels break the header outright; that is also a
		// valid corrupt outcome.
		if media.KindOf(err) != media.KindCorrupt {
			t.Fatalf("header failure kind = %v", media.KindOf(err))
		}
		return
	}
	defer in.Close()
	// The header survived but the essence is cut, so reading must eventually
	// report a corrupt packet rather than silently emit a partial fragment.
	var sawErr error
	for i := 0; i < 100000; i++ {
		if _, err := in.Packet(); err != nil {
			sawErr = err
			break
		}
	}
	if sawErr == nil {
		t.Fatal("truncated MXF produced no packet-level corruption")
	}
	if errors.Is(sawErr, io.EOF) {
		t.Fatal("truncated MXF ended cleanly, corrupt tail not detected")
	}
	if media.KindOf(sawErr) != media.KindCorrupt {
		t.Fatalf("kind = %v (%v)", media.KindOf(sawErr), sawErr)
	}
	if lenorce := sawErr.Error(); len(lenorce) == 0 {
		t.Fatal("empty error message")
	}
}

func TestOpenRejectsNonMedia(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notmedia.bin")
	if err := os.WriteFile(path, []byte("this is not a movie file"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, Options{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("kind = %v", media.KindOf(err))
	}
}

func TestOpenRejectsURL(t *testing.T) {
	_, err := Open("http://example.com/x.mp4", Options{})
	if media.KindOf(err) != media.KindUsage {
		t.Fatalf("kind = %v", media.KindOf(err))
	}
}

func TestPacketsReadAndClose(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var n int
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("packet %d: %v", n, err)
		}
		if len(p.Data) == 0 {
			t.Fatalf("packet %d has empty data", n)
		}
		if p.TrackID == "" {
			t.Fatalf("packet %d has empty track", n)
		}
		n++
		if n > 10000 {
			t.Fatal("too many packets")
		}
	}
	if n == 0 {
		t.Fatal("no packets read")
	}
}

func TestMappingDefaultMapsEveryUsableTrack(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	m, err := in.Mapping(nil, false)
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	if len(m.Tracks) != 2 {
		t.Fatalf("mapped tracks = %d", len(m.Tracks))
	}
	if m.ByID["v0"].Source != 0 {
		t.Fatalf("v0 source = %d", m.ByID["v0"].Source)
	}
}

func TestMappingDuplicateIDRejected(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_, err = in.Mapping([]MapSpec{{TrackID: "x", Source: 0}, {TrackID: "x", Source: 1}}, true)
	var me *MappingError
	if !errors.As(err, &me) || me.Kind != ErrDuplicate {
		t.Fatalf("expected duplicate, got %v", err)
	}
}

func TestMappingDuplicateSourceRejected(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_, err = in.Mapping([]MapSpec{{TrackID: "a", Source: 0}, {TrackID: "b", Source: 0}}, true)
	var me *MappingError
	if !errors.As(err, &me) || me.Kind != ErrDuplicate {
		t.Fatalf("expected duplicate, got %v", err)
	}
}

func TestMappingMissingTrackRejected(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_, err = in.Mapping([]MapSpec{{TrackID: "v0", Source: 0}}, false)
	var me *MappingError
	if !errors.As(err, &me) || me.Kind != ErrMissing {
		t.Fatalf("expected missing, got %v", err)
	}
}

func TestMappingUnknownSourceRejected(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "good.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_, err = in.Mapping([]MapSpec{{TrackID: "z", Source: 99}}, true)
	var me *MappingError
	if !errors.As(err, &me) || me.Kind != ErrUnknown {
		t.Fatalf("expected unknown, got %v", err)
	}
}

func TestBadCropTrackDisabled(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "badcrop.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatalf("open should succeed so the conflict is reported, got %v", err)
	}
	defer in.Close()
	tr, ok := in.Track("v0")
	if !ok {
		t.Fatal("video track missing")
	}
	if !tr.Display.Invalid {
		t.Fatalf("declared 170-wide crop over 160 coded frame should invalidate: %+v", tr.Display)
	}
	// Mapping must refuse to silently emit a track with an illegal crop.
	if _, err := in.Mapping(nil, false); err == nil {
		t.Fatal("mapping should reject the disabled track")
	}
}

func TestBadRotationTrackDisabled(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "badrot.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer in.Close()
	tr, ok := in.Track("v0")
	if !ok {
		t.Fatal("video track missing")
	}
	if !tr.Display.Invalid {
		t.Fatalf("45-degree rotation should invalidate the track: %+v", tr.Display)
	}
	if !strings.Contains(tr.Display.InvalidReason, "90") {
		t.Fatalf("reason = %q", tr.Display.InvalidReason)
	}
}

func TestVFRPacketSpacingIsVariable(t *testing.T) {
	set := fixtureSet(t)
	f := findFixture(t, set, "vfr.mp4")
	in, err := Open(f.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var prev int64 = -1
	distinct := map[int64]bool{}
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.TrackID != "v0" || !p.HasPTS() {
			continue
		}
		if prev >= 0 {
			distinct[p.PTS-prev] = true
		}
		prev = p.PTS
	}
	// A constant-average-rate generator would produce exactly one spacing.
	// Genuine VFR must produce more than one distinct inter-frame gap.
	if len(distinct) < 2 {
		t.Fatalf("expected variable spacing, got %d distinct gaps: %v", len(distinct), distinct)
	}
}
