package manifest

import (
	"strings"
	"testing"

	"github.com/pairwise/mediad/internal/media"
)

func hlsFixture() []byte {
	return []byte(`#EXTM3U
#EXT-X-VERSION:4
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:2.000,
seg-10.m4s
#EXTINF:2.000,
seg-11.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
seg-12.m4s
#EXTINF:2.000,
seg-13.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
seg-14.m4s
#EXT-X-ENDLIST
`)
}

func TestHLSRoundTripFindsTwoDiscontinuities(t *testing.T) {
	canonical, _, reparsed, mismatch, err := RoundTripHLS(hlsFixture())
	if err != nil {
		t.Fatal(err)
	}
	if mismatch != nil {
		t.Fatalf("mismatch: %s", mismatch)
	}
	if len(reparsed.Discontinuities) != 2 {
		t.Fatalf("discontinuities=%v", reparsed.Discontinuities)
	}
	if reparsed.Discontinuities[0] != 12 || reparsed.Discontinuities[1] != 14 {
		t.Fatalf("indexes=%v", reparsed.Discontinuities)
	}
	if strings.Count(string(canonical), "#EXT-X-DISCONTINUITY") != 2 {
		t.Fatal("canonical playlist must retain two tags")
	}
}

func TestHLSRejectsMissingTargetDurationAndHeader(t *testing.T) {
	bad := []byte(strings.Replace(string(hlsFixture()), "#EXT-X-TARGETDURATION:2\n", "", 1))
	_, err := ParseHLS(bad)
	if me, ok := media.AsError(err); !ok || me.Code != media.CodeMissingField || me.Field != "EXT-X-TARGETDURATION" {
		t.Fatalf("err=%v", err)
	}
	if _, err := ParseHLS([]byte("#not-m3u8\n")); media.ExitCode(err) != 2 {
		t.Fatalf("header err exit=%d", media.ExitCode(err))
	}
}

func TestHLSRejectsSegmentWithoutDuration(t *testing.T) {
	bad := []byte(`#EXTM3U
#EXT-X-TARGETDURATION:2
seg-without-inf.m4s
`)
	_, err := ParseHLS(bad)
	if me, ok := media.AsError(err); !ok || me.Code != media.CodeMissingField {
		t.Fatalf("err=%v", err)
	}
}

func TestHLSFirstMismatchAndUnsafeURI(t *testing.T) {
	a, err := ParseHLS(hlsFixture())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseHLS(hlsFixture())
	if err != nil {
		t.Fatal(err)
	}
	b.Segments[3].Duration = 9.5
	if mm := CompareHLS(a, b); mm == nil || mm.Field != "segment[3].duration" {
		t.Fatalf("mismatch=%+v", mm)
	}
	unsafe := []byte(strings.Replace(string(hlsFixture()), "seg-10.m4s", "../escape.m4s", 1))
	_, err = ParseHLS(unsafe)
	if me, ok := media.AsError(err); !ok || me.Code != media.CodePathViolation {
		t.Fatalf("unsafe uri err=%v", err)
	}
}
