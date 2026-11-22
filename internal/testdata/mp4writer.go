package testdata

import (
	"bytes"
	"encoding/binary"
)

// boxWriter serializes a minimal but spec-shaped MP4 using go-mp4's box
// framing for ftyp/moov and hand-written stbl tables. Keeping payloads in
// one mdat keeps the fixture small and fully seekable for the parser.
type boxWriter struct {
	buf      *bytes.Buffer
	offsets  []uint64 // stco chunk offsets, collected while writing mdat
	mdatPos  int64
}

func newBoxWriter() *boxWriter { return &boxWriter{buf: new(bytes.Buffer)} }

func (w *boxWriter) raw(b []byte) { w.buf.Write(b) }

func (w *boxWriter) header(typ string, size int) {
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h, uint32(size))
	copy(h[4:], typ)
	w.raw(h)
}

// begin/end manage nested boxes via a stack of (type, start).
type boxFrame struct {
	typ   string
	start int
}

func (w *boxWriter) begin(typ string) boxFrame {
	f := boxFrame{typ: typ, start: w.buf.Len()}
	w.header(typ, 0) // placeholder size
	return f
}

func (w *boxWriter) end(f boxFrame) {
	end := w.buf.Len()
	size := end - f.start
	binary.BigEndian.PutUint32(w.buf.Bytes()[f.start:], uint32(size))
}

func fullBox(ver uint8, flags uint32) []byte {
	b := []byte{ver, 0, 0, 0}
	b[1] = byte(flags >> 16)
	b[2] = byte(flags >> 8)
	b[3] = byte(flags)
	return b
}

// WriteMP4 serializes the two media tracks plus metadata boxes. Corruption
// switches produce precisely malformed containers for parser error tests.
func WriteMP4(video, audio TrackSpec, evs []TimedEvent, p Params) ([]byte, error) {
	w := newBoxWriter()
	if err := w.writeFtyp(); err != nil {
		return nil, err
	}
	// Top-level emsg boxes (one per emsg-family event), version 0.
	for _, e := range evs {
		if e.Kind == KindEmsg {
			w.writeEmsg(e)
		}
	}
	// mdat before moov (fragmented-style ordering) so stco can carry the real
	// absolute chunk offsets while tables are being written afterwards.
	w.writeMdat(video, audio)
	if err := w.writeMoov(video, audio, evs, p); err != nil {
		return nil, err
	}
	out := w.buf.Bytes()
	if p.Truncate {
		// Cut the final mdat short: its declared size runs past EOF.
		out = out[:len(out)-32]
	}
	if p.CorruptBox {
		// Overwrite the mdat header's declared length with an impossible value
		// that points past EOF, so hierarchy validation must reject it.
		binary.BigEndian.PutUint32(out[w.mdatPos:w.mdatPos+4], uint32(1<<30))
	}
	return out, nil
}

func (w *boxWriter) writeFtyp() error {
	body := []byte("isom") // major brand
	body = binary.BigEndian.AppendUint32(body, 1)
	body = append(body, []byte("isom")...) // compatible brand
	f := w.begin("ftyp")
	w.raw(body)
	w.end(f)
	return nil
}

func (w *boxWriter) writeEmsg(e TimedEvent) {
	payload := make([]byte, e.PayloadLen)
	fill(payload, 9, int(e.ID), false)
	scheme := append([]byte("urn:mq:evt\x00"), []byte("v1\x00")...)
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	body.Write(scheme)
	binary.Write(body, binary.BigEndian, uint32(e.Timebase.Num)) // timescale
	binary.Write(body, binary.BigEndian, uint32(e.RawTicks))
	binary.Write(body, binary.BigEndian, uint32(0)) // duration
	binary.Write(body, binary.BigEndian, e.ID)
	body.Write(payload)
	f := w.begin("emsg")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeMdat(video, audio TrackSpec) {
	f := w.begin("mdat")
	w.mdatPos = int64(f.start)
	// One chunk per sample; remember absolute offsets for stco.
	for _, s := range appendTrackSamples(video, audio) {
		w.offsets = append(w.offsets, uint64(w.buf.Len()))
		w.raw(s)
	}
	w.end(f)
}

func appendTrackSamples(video, audio TrackSpec) [][]byte {
	out := make([][]byte, 0, len(video.Samples)+len(audio.Samples))
	for _, s := range video.Samples {
		out = append(out, s.Payload)
	}
	for _, s := range audio.Samples {
		out = append(out, s.Payload)
	}
	return out
}
