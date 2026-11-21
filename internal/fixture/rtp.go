package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// HLS media playlist: 12 two-second segments of one MP4 byte stream, with a
// deliberate gap at MissingIdx.
func hlsPlaylist(l Layout) []byte {
	out := []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < SegmentCount; i++ {
		if i == l.MissingIdx {
			continue
		}
		out = append(out, []byte(fmt.Sprintf("#EXTINF:2.000,\n%s\n", videoSeg(i)))...)
	}
	out = append(out, []byte("#EXT-X-ENDLIST\n")...)
	return out
}

// DASH MPD: matching video and audio SegmentTemplates with SegmentTimeline.
func dashManifest(l Layout) []byte {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT24S">
  <Period id="p0" start="PT0S">
    <AdaptationSet id="video" contentType="video" mimeType="video/mp4" timescale="%d">
      <SegmentTemplate timescale="%d" initialization="init-v.mp4" media="video/seg-$Number%02d.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="%d" r="%d"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v0" bandwidth="1200000" mimeType="video/mp4"/>
    </AdaptationSet>
    <AdaptationSet id="audio" contentType="audio" mimeType="audio/mp4" timescale="%d">
      <SegmentTemplate timescale="%d" initialization="init-a.mp4" media="audio/seg-$Number%02d.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="%d" r="%d"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="a0" bandwidth="128000" mimeType="audio/mp4"/>
    </AdaptationSet>
  </Period>
</MPD>
`, VideoTimescale, VideoTimescale, 1, SegmentSeconds*VideoTimescale, SegmentCount-1,
		AudioTimescale, AudioTimescale, 1, SegmentSeconds*AudioTimescale, SegmentCount-1)
	return []byte(body)
}

// RTP generation: a 90 kHz video RTP stream over UDP with a controlled loss
// pattern. Every lossEvery-th sequence number is dropped; a small reordering
// window and one duplicate exercise the jitter buffer.

// RTPTimestampStep is the 90 kHz timestamp advance per 30 ms packet.
const RTPTimestampStep = 90_000 * 30 / 1000

// RTPPacket is the on-disk form of one captured packet: 4-byte big-endian
// length followed by the raw RTP bytes.
type RTPPacket struct {
	Seq     uint16
	Lost    bool
	Payload []byte
}

// GenerateRTP writes lossEvery-induced gaps plus a duplicate and swaps a pair
// of packets inside the reorder window. It returns the count of dropped
// packets so the demo can report the expected loss.
func GenerateRTP(path string, packets int, lossEvery int) (dropped int, err error) {
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
	flush := func(p *RTPPacket) error {
		if p == nil {
			return nil
		}
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
	emit := func(p *RTPPacket) error {
		return flush(p)
	}
	for i := 0; i < packets; i++ {
		seq := uint16(i)
		if i > 0 && i%lossEvery == 0 {
			dropped++
			continue // simulate transport loss
		}
		payload := make([]byte, 40)
		for j := range payload {
			payload[j] = byte((i*13 + j*7) % 251)
		}
		pkt := &RTPPacket{Seq: seq, Payload: payload}
		// Hold one packet back to force an out-of-order arrival pair.
		if i == 9 {
			pending = pkt
			continue
		}
		if i == 10 && pending != nil {
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
			// Duplicate of the previous packet, as a retransmission would look.
			if err := emit(pkt); err != nil {
				return dropped, err
			}
		}
	}
	return dropped, flush(pending)
}
