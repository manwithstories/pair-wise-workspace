package hls_test

import (
	"strings"
	"testing"

	avhls "avpipe/internal/hls"
)

func testInput(gapSeq int) avhls.BuildInput {
	in := avhls.BuildInput{PartCount: 2, Diagnostics: map[string]string{"psi_crc_reject": "1"}}
	for ri, id := range []string{"r0_600000", "r1_1200000"} {
		rv := avhls.RenditionView{ID: id, Name: id, Bandwidth: int64(600000 * (ri + 1)), PlaylistFile: id + ".m3u8"}
		for s := 0; s < 12; s++ {
			seg := avhls.SegmentView{
				Seq: s, File: "segments/" + id + "/seg_" + string(rune('0'+s/10)) + string(rune('0'+s%10)) + ".ts",
				Duration: 1, Size: 1880, SHA256: "x",
			}
			// Use ordinary decimal names instead of rune arithmetic clarity.
			seg.File = "segments/" + id + "/seg_" + twoDigit(s) + ".ts"
			if s == gapSeq && ri == 0 {
				seg.Gap = true
				seg.Size = 0
			}
			rv.Segments = append(rv.Segments, seg)
		}
		in.Renditions = append(in.Renditions, rv)
	}
	return in
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestPlaylistRoundTripAndSharedSequence(t *testing.T) {
	in := testInput(7)
	files, err := avhls.Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := avhls.VerifyRoundTrip(files.Master, in)
	if err != nil {
		t.Fatal(err)
	}
	checked, matched := rt.Checked, rt.Matched
	for _, rv := range in.Renditions {
		data := files.Media[rv.PlaylistFile]
		c, m, err := avhls.VerifyMedia(data, rv)
		if err != nil {
			t.Fatal(err)
		}
		checked += c
		matched += m
		if !strings.Contains(string(data), "#EXT-X-PART-INF:PART-TARGET=") {
			t.Fatalf("%s missing LL part info", rv.PlaylistFile)
		}
		if !strings.Contains(string(data), "#EXT-X-SERVER-CONTROL:") {
			t.Fatalf("%s missing server control", rv.PlaylistFile)
		}
	}
	if checked == 0 || matched != checked {
		t.Fatalf("roundtrip matched=%d checked=%d", matched, checked)
	}
	low := string(files.Media[in.Renditions[0].PlaylistFile])
	if !strings.Contains(low, "#EXT-X-GAP") || !strings.Contains(low, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Fatal("gap or shared sequence missing from media playlist")
	}
	if !strings.Contains(string(files.Master), "r0_600000.m3u8") {
		t.Fatal("master playlist missing variant URI")
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	if _, err := avhls.SafeJoin(t.TempDir(), "../evil.m3u8"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := avhls.SafeJoin(t.TempDir(), "/abs/evil.m3u8"); err == nil {
		t.Fatal("absolute path accepted")
	}
}
