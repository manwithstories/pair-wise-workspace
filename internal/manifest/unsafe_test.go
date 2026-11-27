package manifest

import (
	"errors"
	"path/filepath"
	"testing"

	"avrepair/internal/media"
)

func TestDASHRemoteBaseURLIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.m4s", "x")
	mpd := write(t, dir, "remote.mpd", `<MPD><BaseURL>https://cdn.example.com/dash/</BaseURL>
<Period><AdaptationSet contentType="audio"><Representation id="a">
<SegmentTemplate timescale="1000" media="a$Number$.m4s" startNumber="0" duration="2000"/>
</Representation></AdaptationSet></Period></MPD>`)
	_, err := Load(mpd)
	if !errors.Is(err, media.ErrUnsafeURI) {
		t.Fatalf("remote base URL accepted: %v", err)
	}
}

func TestDASHTraversalMediaTemplateIsRejected(t *testing.T) {
	dir := t.TempDir()
	mpd := write(t, dir, "bad.mpd", `<MPD><Period><AdaptationSet contentType="video"><Representation id="v">
<SegmentTemplate timescale="90000" media="../../etc/$Number$.m4s" startNumber="0" duration="90000"/>
</Representation></AdaptationSet></Period></MPD>`)
	_, err := Load(mpd)
	if !errors.Is(err, media.ErrPathTraversal) {
		t.Fatalf("traversal template accepted: %v", err)
	}
}

func TestHLSAbsolutePathRejected(t *testing.T) {
	dir := t.TempDir()
	pl := write(t, dir, "abs.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n/etc/passwd\n#EXT-X-ENDLIST\n")
	_, err := Load(pl)
	if !errors.Is(err, media.ErrUnsafeURI) {
		t.Fatalf("absolute path accepted: %v", err)
	}
}

func TestDASHMissingTimelineWithNoDurationRejected(t *testing.T) {
	dir := t.TempDir()
	mpd := write(t, dir, "nodur.mpd", `<MPD><Period><AdaptationSet contentType="video"><Representation id="v">
<SegmentTemplate timescale="90000" media="v$Number$.m4s" startNumber="0"/>
</Representation></AdaptationSet></Period></MPD>`)
	if _, err := Load(mpd); err == nil {
		t.Fatal("manifest without timeline or duration accepted")
	}
}

func TestDirectoryManifestOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	write(t, root, "outer.ts", "x")
	sub := filepath.Join(root, "case")
	write(t, sub, "inner.ts", "x")
	pl := write(t, sub, "up.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n../outer.ts\n#EXT-X-ENDLIST\n")
	_, err := Load(pl)
	if !errors.Is(err, media.ErrPathTraversal) {
		t.Fatalf("parent-relative segment accepted: %v", err)
	}
}
