package fixture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeHLSPlaylists writes a master playlist plus primary/backup variant
// playlists for one camera.
func writeHLSPlaylists(camRoot, cam string, plan *Plan, count int, segmentSec float64) (string, error) {
	master := filepath.Join(camRoot, "master.m3u8")
	var mb strings.Builder
	mb.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")
	for _, role := range []string{"primary", "backup"} {
		bw := 2000000
		res := "320x180"
		if role == "backup" {
			bw = 800000
		}
		fmt.Fprintf(&mb, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s,NAME=%s\n%s/index.m3u8\n", bw, res, role, role)
	}
	if err := os.WriteFile(master, []byte(mb.String()), 0o644); err != nil {
		return "", err
	}
	for _, role := range []string{"primary", "backup"} {
		if err := writeHLSVariant(camRoot, cam, role, plan, count, segmentSec); err != nil {
			return "", err
		}
	}
	return master, nil
}

func writeHLSVariant(camRoot, cam, role string, plan *Plan, count int, segmentSec float64) error {
	isBackup := role == "backup"
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")

	// Determine the entry sequence numbers; an out-of-order fault swaps two
	// adjacent declared sequences in the primary playlist.
	seqs := make([]int, count)
	for i := range seqs {
		seqs[i] = i
	}
	ooo := plan.FaultsFor(cam, "out_of_order")
	if !isBackup && len(ooo) > 0 {
		k := ooo[0].Sequence
		if k+1 < count {
			seqs[k], seqs[k+1] = seqs[k+1], seqs[k]
		}
	}

	for pos := 0; pos < count; pos++ {
		declared := seqs[pos]
		if plan.HasFault(cam, declared, "pts_reset") && !isBackup {
			b.WriteString("#X-PTS-RESET:true\n")
		}
		fmt.Fprintf(&b, "#X-MEDIA-SEQ:%d\n", declared)
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", segmentSec)
		// The physical file for a slot is named by its true position. A lost
		// slot keeps its playlist entry but has no file on disk; the pipeline
		// detects the missing file and recovers from the backup.
		b.WriteString(segmentFileName(pos) + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	path := filepath.Join(camRoot, role, "index.m3u8")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writeDASHMPD writes a single-period MPD with primary and backup
// AdaptationSets and an explicit SegmentTimeline.
func writeDASHMPD(camRoot, cam string, plan *Plan, count int, segmentSec float64) (string, error) {
	durTicks := int64(segmentSec * 90000)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011">` + "\n")
	b.WriteString(`  <Period id="0">` + "\n")
	for _, role := range []string{"primary", "backup"} {
		isBackup := role == "backup"
		contentType := "video/" + role
		bw := 2000000
		if isBackup {
			bw = 800000
		}
		fmt.Fprintf(&b, `    <AdaptationSet contentType="%s" mimeType="video/mp2t">`+"\n", contentType)
		fmt.Fprintf(&b, `      <SegmentTemplate timescale="90000" media="%s/seg-$Number$.ts" startNumber="0">`+"\n", role)
		b.WriteString(`        <SegmentTimeline>` + "\n")
		for seq := 0; seq < count; seq++ {
			// Every slot is listed; missing files are detected separately so
			// recovery from the backup representation can take over.
			t := int64(seq) * durTicks
			fmt.Fprintf(&b, `          <S t="%d" d="%d"/>`+"\n", t, durTicks)
		}
		b.WriteString(`        </SegmentTimeline>` + "\n")
		b.WriteString(`      </SegmentTemplate>` + "\n")
		fmt.Fprintf(&b, `      <Representation id="%s" codecs="avc1.4d401f,mp4a.40.2" bandwidth="%d" width="%d" height="%d" audioSamplingRate="%d" audioChannelConfiguration="%d"/>`+"\n",
			role, bw, DefaultWidth, DefaultHeight, DefaultSampleRate, DefaultChannels)
		b.WriteString(`    </AdaptationSet>` + "\n")
	}
	b.WriteString(`  </Period>` + "\n")
	b.WriteString(`</MPD>` + "\n")

	path := filepath.Join(camRoot, "manifest.mpd")
	return path, os.WriteFile(path, []byte(b.String()), 0o644)
}
