package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// HLS media playlist: 12 two-second segments with a deliberate gap at
// MissingIdx, plus an init map.
func hlsPlaylist(l Layout) []byte {
	out := []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
	out = append(out, []byte("#EXT-X-MAP:URI=\"media/init-v.mp4\"\n")...)
	// The playlist references every slot, including the one whose media file
	// is deliberately absent, so scanning reports a missing dependency.
	for i := 0; i < SegmentCount; i++ {
		out = append(out, []byte(fmt.Sprintf("#EXTINF:2.000,\n%s\n", SegmentURI(i)))...)
	}
	out = append(out, []byte("#EXT-X-ENDLIST\n")...)
	return out
}

// dashManifest emits matching video and audio SegmentTemplates over the same
// media files, using an explicit SegmentTimeline.
func dashManifest(l Layout) []byte {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT24S">
  <Period id="p0" start="PT0S">
    <AdaptationSet id="video" contentType="video" mimeType="video/mp4" timescale="%d">
      <SegmentTemplate timescale="%d" initialization="media/init-v.mp4" media="media/seg-$Number%%02d.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="%d" r="%d"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v0" bandwidth="1200000" mimeType="video/mp4"/>
    </AdaptationSet>
    <AdaptationSet id="audio" contentType="audio" mimeType="audio/mp4" timescale="%d">
      <SegmentTemplate timescale="%d" initialization="media/init-a.mp4" media="media/seg-$Number%%02d.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="%d" r="%d"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="a0" bandwidth="128000" mimeType="audio/mp4"/>
    </AdaptationSet>
  </Period>
</MPD>
`, VideoTimescale, VideoTimescale, SegmentSeconds*VideoTimescale, SegmentCount-1,
		AudioTimescale, AudioTimescale, SegmentSeconds*AudioTimescale, SegmentCount-1)
	return []byte(body)
}

// RTPTimestampStep is the 90 kHz timestamp advance per 30 ms packet.
const RTPTimestampStep = 90_000 * 30 / 1000

// RTPPacket is the on-disk form of one captured packet.
type RTPPacket struct {
	Seq     uint16
	Payload []byte
}

// GenerateRTP writes a length-prefixed capture exercising loss, duplicates
// and reordering. Every lossEvery-th sequence number is dropped, one packet is
// duplicated, and two packets are held back so they arrive after their
// successors. It returns the number of dropped packets.
func GenerateRTP(path string, packets, lossEvery int) (dropped int, err error) {
	if packets <= 0 || lossEvery <= 1 {
		return 0, fmt.Errorf("fixture: invalid rtp parameters %d/%d", packets, lossEvery)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var pending *RTPPacket
	emit := func(p *RTPPacket) error {
		buf := make([]byte, 12, 12+len(p.Payload))
		buf[0], buf[1] = 0x80, 0x60
		binary.BigEndian.PutUint16(buf[2:], p.Seq)
		binary.BigEndian.PutUint32(buf[4:], uint32(p.Seq)*RTPTimestampStep)
		binary.BigEndian.PutUint32(buf[8:], 0xDEADBEEF)
		buf = append(buf, p.Payload...)
		rec := make([]byte, 4+len(buf))
		binary.BigEndian.PutUint32(rec, uint32(len(buf)))
		copy(rec[4:], buf)
		_, werr := f.Write(rec)
		return werr
	}
	flush := func() error {
		if pending == nil {
			return nil
		}
		err := emit(pending)
		pending = nil
		return err
	}
	for i := 0; i < packets; i++ {
		if i > 0 && i%lossEvery == 0 {
			dropped++ // simulated transport loss
			continue
		}
		payload := make([]byte, 40)
		for j := range payload {
			payload[j] = byte((i*13 + j*7) % 251)
		}
		pkt := &RTPPacket{Seq: uint16(i), Payload: payload}
		// Hold the lower half of each pair so it arrives out of order.
		if i == 9 || i == 13 {
			if err := flush(); err != nil {
				return dropped, err
			}
			pending = pkt
			continue
		}
		if (i == 10 || i == 14) && pending != nil {
			if err := emit(pkt); err != nil {
				return dropped, err
			}
			if err := emit(pending); err != nil {
				return dropped, err
			}
			pending = nil
			continue
		}
		if err := emit(pkt); err != nil {
			return dropped, err
		}
		if i == 4 {
			// A retransmission duplicate of an already-delivered packet.
			if err := emit(pkt); err != nil {
				return dropped, err
			}
		}
	}
	return dropped, flush()
}
