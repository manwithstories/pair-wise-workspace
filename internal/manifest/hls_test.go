package manifest

import (
	"io"
	"strings"
	"testing"

	"mediac/internal/core"
)

const hlsMaster = `#EXTM3U
#EXT-X-VERSION:9
#EXT-X-STREAM-INF:BANDWIDTH=400000,RESOLUTION=640x360,CODECS="avc1.640028",VIDEO="v-low"
low/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720,CODECS="avc1.640028",VIDEO="v-mid"
mid/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1600000,RESOLUTION=1920x1080,CODECS="hvc1.1.6.L120",VIDEO="v-high"
high/index.m3u8
`

func hlsMedia(name string, seq, count int, discAt []int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:9\n")
	b.WriteString("#EXT-X-TARGETDURATION:1\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:" + itoa(seq) + "\n")
	b.WriteString("#EXT-X-MAP:URI=\"" + name + "/init.mp4\"\n")
	for i := 0; i < count; i++ {
		cur := seq + i
		for _, d := range discAt {
			if d == cur {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		}
		b.WriteString("#EXTINF:1.0,\n")
		b.WriteString(name + "/seg" + itoa(cur) + ".m4s\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var sign string
	if n < 0 {
		sign, n = "-", -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return sign + string(digits)
}

func hlsOpener(files map[string]string) func(uri string) (io.ReadCloser, error) {
	return func(uri string) (io.ReadCloser, error) {
		data, ok := files[uri]
		if !ok {
			return nil, core.NewFault(core.FaultMissingInit, core.SevBlocking,
				"no such playlist: "+uri, core.Source{})
		}
		return io.NopCloser(strings.NewReader(data)), nil
	}
}

func TestParseHLSClean(t *testing.T) {
	files := map[string]string{
		"low/index.m3u8":  hlsMedia("low", 100, 24, nil),
		"mid/index.m3u8":  hlsMedia("mid", 100, 24, nil),
		"high/index.m3u8": hlsMedia("high", 100, 24, nil),
	}
	m, err := ParseHLS(t.TempDir(), "master.m3u8", strings.NewReader(hlsMaster),
		hlsOpener(files), HLSScope{StartSeq: 100, EndSeq: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Representations) != 3 || m.SegmentCount() != 72 {
		t.Fatalf("reps=%d segments=%d", len(m.Representations), m.SegmentCount())
	}
	rep := m.Representations[0]
	if rep.ID != "v-low" || rep.TargetBPS != 400000 || rep.InitPath == "" {
		t.Fatalf("rep header wrong: %+v", rep)
	}
	first, last := rep.Segments[0], rep.Segments[23]
	if first.Index != 100 || first.StartMs != 0 || last.Index != 123 || last.StartMs != 23000 {
		t.Fatalf("sequence mapping wrong: %+v %+v", first, last)
	}
	if first.Line < 4 {
		t.Fatalf("line number not captured: %+v", first)
	}
}

func TestParseHLSDiscontinuities(t *testing.T) {
	files := map[string]string{
		"low/index.m3u8": hlsMedia("low", 0, 12, []int{4, 9}),
	}
	master := strings.SplitN(hlsMaster, "\n#EXT-X-STREAM-INF", 2)[0] +
		"\n#EXT-X-STREAM-INF:BANDWIDTH=400000,VIDEO=\"v-low\"\nlow/index.m3u8\n"
	m, err := ParseHLS(t.TempDir(), "master.m3u8", strings.NewReader(master),
		hlsOpener(files), HLSScope{StartSeq: 0, EndSeq: -1})
	if err != nil {
		t.Fatal(err)
	}
	disc := m.Representations[0].Discontinuities
	if len(disc) != 2 || disc[0].Index != 4 || disc[1].Index != 9 {
		t.Fatalf("discontinuities=%+v", disc)
	}
	if disc[0].StartMs != 4000 || disc[1].StartMs != 9000 {
		t.Fatalf("unified clock at discontinuity wrong: %+v", disc)
	}
	if disc[0].Line == 0 {
		t.Fatal("discontinuity line number missing")
	}
}

func TestParseHLSRejects(t *testing.T) {
	t.Run("escape in master", func(t *testing.T) {
		bad := strings.Replace(hlsMaster, "low/index.m3u8", "../evil.m3u8", 1)
		files := map[string]string{
			"mid/index.m3u8":  hlsMedia("mid", 0, 2, nil),
			"high/index.m3u8": hlsMedia("high", 0, 2, nil),
		}
		_, err := ParseHLS(t.TempDir(), "master.m3u8", strings.NewReader(bad),
			hlsOpener(files), HLSScope{StartSeq: 0, EndSeq: -1})
		if f, ok := core.AsFault(err); !ok || f.Kind != core.FaultPathEscape || f.Source.Line == 0 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missing map", func(t *testing.T) {
		noMap := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\na.m4s\n"
		files := map[string]string{"low/index.m3u8": noMap}
		master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,VIDEO=\"v-low\"\nlow/index.m3u8\n"
		_, err := ParseHLS(t.TempDir(), "master.m3u8", strings.NewReader(master),
			hlsOpener(files), HLSScope{StartSeq: 0, EndSeq: -1})
		if f, ok := core.AsFault(err); !ok || f.Kind != core.FaultMissingInit {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("backward segment", func(t *testing.T) {
		// Negative EXTINF produces a cursor regression that must be rejected.
		backward := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-MAP:URI=\"a/init.mp4\"\n" +
			"#EXTINF:1.0,\na/s0.m4s\n#EXTINF:-1.0,\na/s1.m4s\n#EXTINF:1.0,\na/s2.m4s\n"
		files := map[string]string{"low/index.m3u8": backward}
		master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,VIDEO=\"v-low\"\nlow/index.m3u8\n"
		_, err := ParseHLS(t.TempDir(), "master.m3u8", strings.NewReader(master),
			hlsOpener(files), HLSScope{StartSeq: 0, EndSeq: -1})
		if f, ok := core.AsFault(err); !ok || f.Kind != core.FaultTimestampBackward {
			t.Fatalf("err=%v", err)
		}
	})
}
