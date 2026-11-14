package playlist

import (
	"bytes"
	"strings"
	"testing"
)

func TestAttributesQuotesAndOrder(t *testing.T) {
	attrs, err := ParseAttributes(`BANDWIDTH=1234,NAME="1080p, main",CODECS="avc1.4d401f",NOTE="a\\\"b"`)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := attrs.Get("NAME"); got != "1080p, main" {
		t.Fatalf("NAME = %q", got)
	}
	if got := attrs.String(); !strings.HasPrefix(got, `BANDWIDTH=1234,NAME="1080p, main"`) {
		t.Fatalf("serialized attributes = %s", got)
	}
	attrs.Add("NEW", "value", true)
	items := attrs.Items()
	if items[len(items)-1].Key != "NEW" || !items[len(items)-1].Quoted {
		t.Fatalf("insertion order or quoting lost: %+v", items)
	}
}

func TestMediaPlaylistRoundTripAndFormatting(t *testing.T) {
	input := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:9
#EXT-X-DISCONTINUITY-SEQUENCE:1
#EXT-X-DISCONTINUITY
#EXTINF:1.875,
seg-9.ts
#EXTINF:2,
seg-10.ts
#EXT-X-ENDLIST
`
	media, err := ParseMedia(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if media.MediaSequence != 9 || len(media.Segments) != 2 || !media.Segments[0].Discontinuity || !media.EndList {
		t.Fatalf("unexpected parse: %+v", media)
	}
	var out bytes.Buffer
	if err := media.Write(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "#EXTINF:1.875,\nseg-9.ts\n") || !strings.Contains(text, "#EXTINF:2.000,\nseg-10.ts\n") {
		t.Fatalf("EXTINF formatting wrong:\n%s", text)
	}
	if strings.Index(text, "#EXT-X-TARGETDURATION") > strings.Index(text, "#EXT-X-MEDIA-SEQUENCE") {
		t.Fatalf("media tag order changed:\n%s", text)
	}
}

func TestMasterPlaylistRoundTrip(t *testing.T) {
	input := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=500000,AVERAGE-BANDWIDTH=450000,RESOLUTION=1920x1080,NAME="1080p"
1080p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=300000,AVERAGE-BANDWIDTH=280000,RESOLUTION=1280x720,NAME="720p"
720p/index.m3u8
`
	master, err := ParseMaster(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !master.IndependentSegments || len(master.Variants) != 2 || master.Variants[0].Bandwidth != 500000 {
		t.Fatalf("unexpected parse: %+v", master)
	}
	var out bytes.Buffer
	if err := master.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `#EXT-X-STREAM-INF:BANDWIDTH=500000,AVERAGE-BANDWIDTH=450000,RESOLUTION=1920x1080,NAME="1080p"`) {
		t.Fatalf("attribute order changed:\n%s", out.String())
	}
}
