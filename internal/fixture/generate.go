// Package fixture synthesizes reproducible archive layouts: HLS and
// DASH manifests plus self-contained fMP4 segments. Everything is
// derived from the options, so the same options always yield identical
// bytes and timestamps; no external media is referenced.
package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"archive/internal/demux"
	"archive/internal/fault"
	"archive/internal/remux"
)

// Segment duration is fixed at two seconds to keep timing exact.
const (
	SegDurSecs  = 2
	VideoTimesc = 90000
	AudioTimesc = 48000
	videoFrames = 50 // 25 fps over 2s
	audioFrames = 100
	// Audio starts 19 ticks (≈0.396 ms) after video, a realistic
	// capture skew that the metrics module must measure, not assume.
	AudioLeadTicks = 19
)

// Options controls the generated layout. Each *At field selects one
// segment index; use -1 (the zero value) for "not applicable".
type Options struct {
	Segments int  // number of media segments
	Wrap     bool // start counters just before their wrap boundary
	// TruncateAt, when non-negative, cuts that segment file short.
	TruncateAt int
	// MissingAt, when non-negative, omits that segment file.
	MissingAt int
	// DuplicateAt, when non-negative, adds a second manifest reference
	// to that segment (same content, distinct playlist entry).
	DuplicateAt int
	// ConflictAt, when non-negative, appends a second video track that
	// collides with the first on (role, language, codec).
	ConflictAt int
}

// Layout describes the files written to the root.
type Layout struct {
	Root     string
	HLS      string // relative path of the HLS playlist
	DASH     string // relative path of the DASH manifest
	MediaDir string // relative directory holding segments
}

func (o Options) normalize() Options {
	if o.Segments <= 0 {
		o.Segments = 1
	}
	return o
}

// Generate writes the layout under root and returns its description.
func Generate(root string, opt Options) (Layout, error) {
	opt = opt.normalize()
	if opt.Segments > 100000 {
		return Layout{}, fault.Newf(fault.Fatal, "fixture", root, "too many segments")
	}
	lay := Layout{Root: root, HLS: "media.m3u8", DASH: "manifest.mpd", MediaDir: "media"}
	for _, d := range []string{filepath.Join(root, lay.MediaDir), filepath.Join(root, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Layout{}, fault.Wrap(fault.Fatal, "fixture", d, err)
		}
	}

	// With Wrap the stored counters restart mid-stream; the archive base
	// tag preserves the true presentation start so the timeline stays
	// continuous without inventing cross-run state.
	// The counter restart happens after wrapAt seconds, so stored
	// counter zero corresponds to that presentation time.
	archiveBase := 0
	if opt.Wrap {
		archiveBase = 40 * SegDurSecs
	}
	var hls bytes.Buffer
	hls.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n")
	hls.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	fmt.Fprintf(&hls, "#EXT-X-ARCHIVE-BASE:%d\n", archiveBase)
	emitted := 0 // manifest entries, including the duplicate
	for i := 0; i < opt.Segments; i++ {
		name := fmt.Sprintf("seg-%05d.m4s", i)
		rel := lay.MediaDir + "/" + name
		data, err := segment(i, opt)
		if err != nil {
			return Layout{}, err
		}
		if i != opt.MissingAt {
			if err := writeFile(filepath.Join(root, rel), data); err != nil {
				return Layout{}, err
			}
		}
		if i == opt.DuplicateAt {
			// A zero-duration repeat of the same URI reproduces a
			// duplicate manifest reference without shifting the clock:
			// it shares the primary entry's start and is deduplicated.
			fmt.Fprintf(&hls, "#EXTINF:0.000000,\n%s/%s\n", lay.MediaDir, name)
			emitted++
		}
		fmt.Fprintf(&hls, "#EXTINF:%d.000000,\n%s/%s\n", SegDurSecs, lay.MediaDir, name)
		emitted++
	}
	hls.WriteString("#EXT-X-ENDLIST\n")
	if err := writeFile(filepath.Join(root, lay.HLS), hls.Bytes()); err != nil {
		return Layout{}, err
	}
	mpd := dashManifest(opt.Segments, opt.DuplicateAt, emitted)
	if err := writeFile(filepath.Join(root, lay.DASH), mpd); err != nil {
		return Layout{}, err
	}
	return lay, nil
}

// dashManifest emits a SegmentTemplate timeline covering every
// generated segment. Duplicate references are an HLS-only anomaly here;
// the DASH form stays one-to-one with the stored media.
func dashManifest(n, dup, emitted int) []byte {
	_ = dup
	_ = emitted
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<MPD type="static"><Period><AdaptationSet mimeType="video/mp4">`+
		`<SegmentTemplate timescale="%d" media="media/seg-$Number$05d$.m4s" startNumber="0">`,
		VideoTimesc)
	b.WriteString(`<SegmentTimeline>`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<S t="%d" d="%d"/>`, i*SegDurSecs*VideoTimesc, SegDurSecs*VideoTimesc)
	}
	b.WriteString(`</SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`)
	return b.Bytes()
}

// segment builds one self-contained fMP4 (ftyp + moov + moof + mdat)
// so every segment can be demuxed on its own.
func segment(idx int, opt Options) ([]byte, error) {
	vbase, abase := segmentBases(idx, opt)
	video := track(idx, 1, "vide", "und", "avc1", VideoTimesc, videoFrames, 3600, vbase)
	video.Entry = avcEntry(1280, 720)
	audio := track(idx, 2, "soun", "eng", "mp4a", AudioTimesc, audioFrames, 960, abase)
	audio.Entry = mp4aEntry(2, 48000, 16)

	inputs := []remux.Input{{Track: video, DTS: videoDTS(vbase)}, {Track: audio, DTS: audioDTS(abase)}}
	if idx == opt.ConflictAt {
		// Same role/language/codec as the first video track: the track
		// mapper must reject, never overwrite.
		dup := video
		dup.ID = 3
		inputs = append(inputs, remux.Input{Track: dup, DTS: videoDTS(vbase)})
	}
	body, _, err := remux.Write(inputs)
	if err != nil {
		return nil, err
	}
	if idx == opt.TruncateAt {
		// Cut inside the media data, leaving a parseable moof but
		// incomplete samples.
		cut := len(body) * 6 / 10
		if cut < len(body)/2 {
			cut = len(body) / 2
		}
		body = body[:cut]
	}
	return body, nil
}

func videoDTS(base int64) []int64 {
	ds := make([]int64, videoFrames)
	for i := range ds {
		ds[i] = base + int64(i)*3600
	}
	return ds
}

func audioDTS(base int64) []int64 {
	ds := make([]int64, audioFrames)
	for i := range ds {
		ds[i] = base + int64(i)*960
	}
	return ds
}

func track(seg int, id uint32, handler, lang, codec string, timescale uint32, n int, dur uint32, base int64) demux.Track {
	tr := demux.Track{ID: id, Timescale: timescale, Lang: lang, Codec: codec, Handler: handler}
	for i := 0; i < n; i++ {
		// Segment index and sample index are embedded in every payload
		// so segments never collide in the CAS by accident.
		p := make([]byte, 64)
		binary.BigEndian.PutUint32(p[0:], uint32(seg))
		binary.BigEndian.PutUint32(p[4:], uint32(i))
		copy(p[8:], []byte(fmt.Sprintf("%s-%04d-%04d", codec, seg, i)))
		cts := int64(0)
		// Interior even samples carry a presentation offset, modelling
		// decoder reordering while keeping the first and last samples
		// (the measured boundaries) exact.
		if handler == "vide" && i%2 == 0 && i > 0 && i < n-1 {
			cts = 3600
		}
		tr.Samples = append(tr.Samples, demux.Sample{
			DTS: uint64(base + int64(i)*int64(dur)), Dur: dur, CTSOff: cts, Data: p,
		})
	}
	if handler == "vide" {
		tr.Width, tr.Height = 1280, 720
	} else {
		tr.Channels, tr.SampleBits, tr.SampleRate = 2, 16, 48000
	}
	return tr
}

// avcEntry builds an avc1 visual sample entry with a minimal avcC.
func avcEntry(w, h uint16) []byte {
	var p bytes.Buffer
	p.Write(make([]byte, 6)) // reserved
	u16(&p, 1)               // data reference index
	u16(&p, 0)               // pre-defined
	u16(&p, 0)               // reserved
	for i := 0; i < 3; i++ {
		u32(&p, 0)
	}
	u16(&p, w)
	u16(&p, h)
	u32(&p, 0x00480000)       // 72 dpi horizontal
	u32(&p, 0x00480000)       // 72 dpi vertical
	u32(&p, 0)                // reserved
	u16(&p, 1)                // frame count
	p.Write(make([]byte, 32)) // compressor name
	u16(&p, 0x0018)           // depth
	u16(&p, 0xffff)           // pre-defined -1
	// avcC: baseline profile, one SPS and one PPS.
	sps := []byte{0x67, 0x42, 0x00, 0x1e, 0x8c, 0x8d, 0x40}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	var c bytes.Buffer
	c.Write([]byte{0x01, 0x42, 0x00, 0x1e, 0xff, 0xe1})
	u16(&c, uint16(len(sps)))
	c.Write(sps)
	c.WriteByte(1)
	u16(&c, uint16(len(pps)))
	c.Write(pps)
	p.Write(box("avcC", c.Bytes()))
	return box("avc1", p.Bytes())
}

// mp4aEntry builds an mp4a audio sample entry.
func mp4aEntry(ch, bits uint16, rate uint32) []byte {
	var p bytes.Buffer
	p.Write(make([]byte, 6)) // reserved
	u16(&p, 1)               // data reference index
	u16(&p, 0)               // version
	u16(&p, 0)               // revision
	u32(&p, 0)               // vendor
	u16(&p, ch)
	u16(&p, bits)
	u16(&p, 0)        // compression id
	u16(&p, 0)        // packet size
	u32(&p, rate<<16) // 16.16 sample rate
	return box("mp4a", p.Bytes())
}

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func u16(b *bytes.Buffer, v uint16) {
	var x [2]byte
	binary.BigEndian.PutUint16(x[:], v)
	b.Write(x[:])
}

func u32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fault.Wrap(fault.Fatal, "fixture", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fault.Wrap(fault.Fatal, "fixture", path, err)
	}
	return nil
}
