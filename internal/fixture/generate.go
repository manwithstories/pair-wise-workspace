// Package fixture generates every sample the demo and acceptance flow need.
// No external media is referenced: all bytes are synthesized locally and
// deterministically, so a fresh checkout can run the full demo offline.
package fixture

import (
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/pion/rtp"
)

// Set lists the files produced by Generate.
type Set struct {
	Dir string
	// Healthy inputs.
	GoodSRT string
	GoodVTT string
	GoodMP4 string
	GoodRTP string
	// Damaged / edge-case inputs.
	HalfLineSRT  string // cue payload truncated mid-line, no newline
	WrapSRT      string // cues cross the 24:00:00 boundary
	TruncatedMP4 string // mdat box cut short
}

// Generate writes all fixtures into dir/inputs and returns their paths.
func Generate(dir string) (*Set, error) {
	in := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(in, 0o755); err != nil {
		return nil, err
	}
	s := &Set{Dir: in}
	var err error

	if s.GoodSRT, err = write(in, "good.srt", goodSRT()); err != nil {
		return nil, err
	}
	if s.GoodVTT, err = write(in, "good.vtt", goodVTT()); err != nil {
		return nil, err
	}
	if s.HalfLineSRT, err = write(in, "halfline.srt", halfLineSRT()); err != nil {
		return nil, err
	}
	if s.WrapSRT, err = write(in, "wrap.srt", wrapSRT()); err != nil {
		return nil, err
	}
	mp4 := goodMP4()
	if s.GoodMP4, err = write(in, "good.mp4", mp4); err != nil {
		return nil, err
	}
	if s.TruncatedMP4, err = write(in, "truncated.mp4", truncatedMP4(mp4)); err != nil {
		return nil, err
	}
	if s.GoodRTP, err = write(in, "good.rtp", goodRTP()); err != nil {
		return nil, err
	}
	return s, nil
}

func write(dir, name string, data []byte) (string, error) {
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, data, 0o600)
}

// box encodes one MP4 box (size + type + payload).
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// stss encodes a sync-sample box with the given 1-based sample numbers.
func stss(samples ...uint32) []byte {
	payload := make([]byte, 8+4*len(samples))
	binary.BigEndian.PutUint32(payload[4:], uint32(len(samples)))
	for i, n := range samples {
		binary.BigEndian.PutUint32(payload[8+i*4:], n)
	}
	return box("stss", payload)
}

func goodSRT() []byte {
	return []byte(
		"1\n00:00:01,000 --> 00:00:02,000\ncamera one ready\n\n" +
			"2\n00:00:03,000 --> 00:00:04,500\naudio levels nominal\n\n" +
			"3\n00:00:05,000 --> 00:00:07,250\nthird cue complete\n\n" +
			"4\n00:00:08,000 --> 00:00:09,000\nfourth cue\n")
}

func goodVTT() []byte {
	return []byte("WEBVTT - live show\n\n" +
		"cue-1\n00:01.000 --> 00:02.000\nopening slate\n\n" +
		"00:03.000 --> 00:04.000\nsecond cue\n")
}

// halfLineSRT ends with a complete cue followed by a cue whose final payload
// line is cut without a trailing newline.
func halfLineSRT() []byte {
	return []byte(
		"1\n00:00:01,000 --> 00:00:02,000\nintact cue\n\n" +
			"2\n00:00:03,000 --> 00:00:04,000\ncut mid wo")
}

// wrapSRT places two cues across midnight: 23:59:59 then 00:00:01, which the
// pipeline's clock resolves as day-1 rather than a backwards jump.
func wrapSRT() []byte {
	return []byte(
		"1\n23:59:59,000 --> 23:59:59,500\nlast cue of day\n\n" +
			"2\n00:00:01,000 --> 00:00:02,000\nfirst cue after midnight\n")
}

func goodMP4() []byte {
	var out []byte
	out = append(out, box("ftyp", []byte("isom\x00\x00\x00\x00isom"))...)
	stbl := append(stss(1, 26, 51, 76), box("stsz", make([]byte, 12))...)
	moov := box("moov",
		box("trak", box("mdia", box("minf", box("stbl", stbl)))))
	out = append(out, moov...)
	out = append(out, box("mdat", make([]byte, 256))...)
	return out
}

// truncatedMP4 cuts the mdat payload 40 bytes early, keeping every earlier
// box (including the stss index) fully readable.
func truncatedMP4(full []byte) []byte {
	mdat := -1
	for i := 0; i+8 <= len(full); i++ {
		if string(full[i+4:i+8]) == "mdat" {
			mdat = i
			break
		}
	}
	if mdat < 0 {
		return full
	}
	cut := mdat + 8 + 40
	if cut > len(full) {
		cut = len(full)
	}
	return append([]byte(nil), full[:cut]...)
}

// goodRTP emits length-prefixed RTP packets: an in-window reorder (100,102,
// 101,103), a duplicate (103 again), then a forward jump that opens a gap
// wider than the reorder window.
func goodRTP() []byte {
	seqs := []uint16{100, 102, 101, 103, 103, 200}
	var out []byte
	for _, seq := range seqs {
		pkt := rtp.Packet{
			Header:  rtp.Header{SequenceNumber: seq, SSRC: 0x5151, PayloadType: 96},
			Payload: []byte{0xaa, 0xbb},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			panic(err)
		}
		rec := make([]byte, 2+len(raw))
		binary.BigEndian.PutUint16(rec, uint16(len(raw)))
		copy(rec[2:], raw)
		out = append(out, rec...)
	}
	return out
}
