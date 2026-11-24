package fixture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediac/internal/transport"
)

// buildClean materializes the defect-free reference sample: 3 reps, 24
// fragments each, matching the acceptance thresholds exactly.
func buildClean(t *Tree) error {
	t.InitFiles = map[string]string{}
	t.FragmentDir = map[string]string{}
	t.HLSMedia = map[string]string{}
	var rtpFrames [][]byte
	for _, spec := range Layout {
		repDir, err := t.mkdir(spec.ID)
		if err != nil {
			return err
		}
		t.FragmentDir[spec.ID] = repDir
		initPath := filepath.Join(repDir, "init.mp4")
		if err := os.WriteFile(initPath, buildInitSegment(spec), 0o644); err != nil {
			return err
		}
		t.InitFiles[spec.ID] = initPath

		for seq := 1; seq <= SegmentCount; seq++ {
			baseDecode := uint64((seq - 1) * SamplesPerFragment * SampleDuration)
			data := mediaFragment(spec, seq, baseDecode)
			fragPath := filepath.Join(repDir, fmt.Sprintf("seg-%d.m4s", seq))
			if err := os.WriteFile(fragPath, data, 0o644); err != nil {
				return err
			}
			frames, err := fragmentRTPFrames(spec.ID, seq, data)
			if err != nil {
				return err
			}
			rtpFrames = append(rtpFrames, frames...)
		}
	}
	if err := writeDASH(t); err != nil {
		return err
	}
	if err := writeHLS(t); err != nil {
		return err
	}
	recordPath := filepath.Join(t.Root, "capture.rtpr")
	if err := writeRTP(recordPath, rtpFrames); err != nil {
		return err
	}
	t.RTPRecord = recordPath
	return nil
}

// fragmentRTPFrames splits one fragment into ordered, non-overlapping RTP
// packets whose extension carries the fragment index and payload offset.
func fragmentRTPFrames(repID string, fragment int, data []byte) ([][]byte, error) {
	// Small MTU guarantees multiple RTP packets per fragment so reordering and
	// burst-loss injection have real packets to act on.
	const mtu = 64
	var frames [][]byte
	var seq uint16 = uint16(fragment * 100)
	for off := 0; off < len(data); off += mtu {
		end := off + mtu
		if end > len(data) {
			end = len(data)
		}
		pkt, err := transport.NewPacket(seq, fragment, uint32(off), data[off:end])
		if err != nil {
			return nil, err
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		frame, err := transport.EncodeFrame(repID, fragment,
			uint32(fragment*1000+int(seq)), raw)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		seq++
	}
	return frames, nil
}

func writeRTP(path string, frames [][]byte) error {
	var all []byte
	for _, f := range frames {
		all = append(all, f...)
	}
	return os.WriteFile(path, all, 0o644)
}

func writeDASH(t *Tree) error {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\n")
	b.WriteString(`<MPD type="static">` + "\n  <Period>\n")
	b.WriteString(`    <AdaptationSet contentType="video" mimeType="video/mp4">` + "\n")
	for _, spec := range Layout {
		fmt.Fprintf(&b,
			"      <Representation id=\"%s\" codecs=\"%s\" width=\"%d\" height=\"%d\" bandwidth=\"%d\">\n",
			spec.ID, spec.Codec, spec.Width, spec.Height, spec.BPS)
		b.WriteString("        <SegmentTemplate timescale=\"1000\" " +
			"initialization=\"" + spec.ID + "/init.mp4\" " +
			"media=\"" + spec.ID + "/seg-$Number$.m4s\" startNumber=\"1\" duration=\"1000\"/>\n")
		b.WriteString("      </Representation>\n")
	}
	b.WriteString("    </AdaptationSet>\n  </Period>\n</MPD>\n")
	path := filepath.Join(t.Root, "manifest.mpd")
	t.DASHMPD = path
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeHLS(t *Tree) error {
	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:9\n")
	for _, spec := range Layout {
		fmt.Fprintf(&master,
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\",VIDEO=\"%s\"\n",
			spec.BPS, spec.Width, spec.Height, spec.Codec, spec.ID)
		fmt.Fprintf(&master, "%s/index.m3u8\n", spec.ID)
	}
	masterPath := filepath.Join(t.Root, "master.m3u8")
	t.HLSMaster = masterPath
	if err := os.WriteFile(masterPath, []byte(master.String()), 0o644); err != nil {
		return err
	}
	for _, spec := range Layout {
		var pl strings.Builder
		pl.WriteString("#EXTM3U\n#EXT-X-VERSION:9\n#EXT-X-TARGETDURATION:1\n")
		pl.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n")
		fmt.Fprintf(&pl, "#EXT-X-MAP:URI=\"init.mp4\"\n")
		for seq := 1; seq <= SegmentCount; seq++ {
			pl.WriteString("#EXTINF:1.000,\n")
			fmt.Fprintf(&pl, "seg-%d.m4s\n", seq)
		}
		pl.WriteString("#EXT-X-ENDLIST\n")
		mediaPath := filepath.Join(t.FragmentDir[spec.ID], "index.m3u8")
		t.HLSMedia[spec.ID] = mediaPath
		if err := os.WriteFile(mediaPath, []byte(pl.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}
