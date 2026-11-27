package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const masterM3U8 = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,NAME=primary
primary/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,NAME=backup
backup/index.m3u8
`

func mediaPlaylist(seq []int, durations []float64, disposition map[int]string) string {
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n"...)
	for i, s := range seq {
		if d, ok := disposition[s]; ok {
			b = append(b, "#X-DISPOSITION:"+d+"\n"...)
		}
		b = append(b, "#X-MEDIA-SEQ:"+itoa(s)+"\n"...)
		b = append(b, "#EXTINF:"+itoaFloat(durations[i])+",\n"...)
		// File name is unique per entry position so two entries can still
		// carry the same declared sequence number.
		b = append(b, "seg-pos-"+itoa(i)+".ts\n"...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return string(b)
}

func TestParseHLSHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "master.m3u8"), masterM3U8)
	seqs := []int{0, 1, 2, 3}
	durs := []float64{2, 2, 2, 2}
	writeFile(t, filepath.Join(dir, "primary", "index.m3u8"), mediaPlaylist(seqs, durs, nil))
	writeFile(t, filepath.Join(dir, "backup", "index.m3u8"), mediaPlaylist(seqs, durs, nil))

	m, err := ParseHLS("cam1", filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Variants) != 2 || len(m.Representations) != 2 {
		t.Fatalf("variants=%d reps=%d", len(m.Variants), len(m.Representations))
	}
	prim := m.Representation("cam1/primary")
	if prim == nil || len(prim.Segments) != 4 {
		t.Fatalf("primary representation wrong: %+v", prim)
	}
	// Start times accumulate on the 90 kHz clock.
	if prim.Segments[2].Start90k != 360000 {
		t.Fatalf("seg2 start = %d, want 360000", prim.Segments[2].Start90k)
	}
	if prim.Width != 1280 || prim.Height != 720 {
		t.Fatalf("resolution = %dx%d", prim.Width, prim.Height)
	}
}

func TestParseHLSRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	bad := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n../../etc/secret.ts\n#EXT-X-ENDLIST\n"
	writeFile(t, filepath.Join(dir, "primary", "index.m3u8"), bad)
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,NAME=primary\nprimary/index.m3u8\n"
	writeFile(t, filepath.Join(dir, "master.m3u8"), master)

	if _, err := ParseHLS("cam", filepath.Join(dir, "master.m3u8")); !errors.Is(err, ErrTraversal) {
		t.Fatalf("err = %v, want ErrTraversal", err)
	}
}

func TestParseHLSRejectsDuplicateAndOrder(t *testing.T) {
	dir := t.TempDir()
	dup := mediaPlaylist([]int{0, 1, 1}, []float64{2, 2, 2}, nil)
	writeFile(t, filepath.Join(dir, "p", "index.m3u8"), dup)
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,NAME=p\np/index.m3u8\n"
	writeFile(t, filepath.Join(dir, "master.m3u8"), master)
	if _, err := ParseHLS("c", filepath.Join(dir, "master.m3u8")); !errors.Is(err, ErrDuplicateSeq) {
		t.Fatalf("dup err = %v, want ErrDuplicateSeq", err)
	}

	ooo := mediaPlaylist([]int{0, 2, 1}, []float64{2, 2, 2}, nil)
	writeFile(t, filepath.Join(dir, "p", "index.m3u8"), ooo)
	if _, err := ParseHLS("c", filepath.Join(dir, "master.m3u8")); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("order err = %v, want ErrOutOfOrder", err)
	}
}

const sampleMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
  <Period id="0">
    <AdaptationSet contentType="video/primary" mimeType="video/mp4">
      <SegmentTemplate timescale="90000" media="primary/$Number$.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="180000" r="2"/>
          <S t="540000" d="180000"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="primary" codecs="avc1.4d401f" bandwidth="2000000" width="1280" height="720"/>
    </AdaptationSet>
    <AdaptationSet contentType="video/backup" mimeType="video/mp4">
      <SegmentTemplate timescale="90000" media="backup/$Number$.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="180000" r="3"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="backup" codecs="avc1.4d401f" bandwidth="800000" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest.mpd"), sampleMPD)
	m, err := ParseDASH("cam2", filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Representations) != 2 {
		t.Fatalf("reps = %d", len(m.Representations))
	}
	prim := m.Representation("cam2/primary")
	if prim == nil || prim.Role != "primary" || len(prim.Segments) != 4 {
		t.Fatalf("primary wrong: %+v", prim)
	}
	// r="2" expands to three entries, then the explicit S gives the fourth.
	last := prim.Segments[3]
	if last.Sequence != 4 || last.Start90k != 540000 || last.Duration90k != 180000 {
		t.Fatalf("last segment wrong: %+v", last)
	}
}

func TestParseDispatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.txt"), "noop")
	if _, err := Parse("c", filepath.Join(dir, "x.txt")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("dispatch err = %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func itoaFloat(f float64) string {
	return itoa(int(f)) + ".0"
}
