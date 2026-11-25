package demux

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediaarchive/internal/model"
)

// Archive metadata box ("armd") lives inside moov and carries the exact
// track/sample tables. Binary layout (big endian), length-prefixed so the box
// can be skipped by generic parsers:
//
//	[4] magic 'ARMD'  [1] version (=1) [1] flags [2] track count N
//	N * track records:
//	  [4] track id  [1] kind  [4] timescale  [8] start time
//	  [4] drift ppm [4] wrap units  [4] rtp count
//	  [1] codec hint len K  [K] codec hint
//	[4] chunk count C
//	C * chunk tables:
//	  [4] chunk seq  [4] sample count M
//	  M * sample records:
//	    [4] track id  [4] seq  [8] dts  [4] duration
//	    [8] payload offset  [4] size  [4] flags  [4] rtp seq
const armdMagic = "ARMD"
const armdVersion = 1

// armf precedes each fragmented mdat: [4] 'ARMF' [4] chunk seq [4] sample count.
const armfMagic = "ARMF"

var (
	boxTypeArmd = mp4.StrToBoxType("armd")
	boxTypeArmf = mp4.StrToBoxType("armf")
	boxTypeMdat = mp4.BoxTypeMdat()
	boxTypeMoov = mp4.BoxTypeMoov()
	boxTypeFtyp = mp4.BoxTypeFtyp()
)

type rd struct {
	b   []byte
	err error
}

func (r *rd) byte() byte {
	if r.err != nil || len(r.b) < 1 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *rd) bytes(n int) []byte {
	if r.err != nil || len(r.b) < n {
		r.err = io.ErrUnexpectedEOF
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *rd) u16() uint16 { return binary.BigEndian.Uint16(r.bytes(2)) }
func (r *rd) u32() uint32 { return binary.BigEndian.Uint32(r.bytes(4)) }
func (r *rd) u64() uint64 { return binary.BigEndian.Uint64(r.bytes(8)) }

// parseArmd decodes the archive metadata payload into tracks and sample
// tables. All bounds failures become unrecoverable demux errors: a corrupt
// index cannot be guessed.
func parseArmd(payload []byte) ([]TrackMeta, [][]Sample, error) {
	r := &rd{b: payload}
	if string(r.bytes(4)) != armdMagic {
		return nil, nil, model.NewError(model.StageDemux, "bad armd magic")
	}
	if v := r.byte(); v != armdVersion {
		return nil, nil, model.NewError(model.StageDemux,
			fmt.Sprintf("unsupported armd version %d", v))
	}
	r.byte() // flags
	trackN := int(r.u16())
	tracks := make([]TrackMeta, 0, trackN)
	idx := map[uint32]int{}
	for i := 0; i < trackN; i++ {
		tm := TrackMeta{
			ID:        r.u32(),
			Kind:      kindString(r.byte()),
			Timescale: r.u32(),
			StartTime: r.u64(),
			DriftPPM:  int32(r.u32()),
			WrapUnits: r.u32(),
			RTPCount:  r.u32(),
		}
		k := int(r.byte())
		tm.CodecHint = string(r.bytes(k))
		if r.err != nil {
			break
		}
		if _, dup := idx[tm.ID]; dup {
			return nil, nil, model.NewError(model.StageDemux,
				fmt.Sprintf("duplicated track id %d in armd", tm.ID)).WithTrack(int32(tm.ID))
		}
		idx[tm.ID] = len(tracks)
		tracks = append(tracks, tm)
	}
	chunkN := int(r.u32())
	chunks := make([][]Sample, 0, chunkN)
	for c := 0; c < chunkN; c++ {
		seq := r.u32()
		m := int(r.u32())
		samps := make([]Sample, 0, m)
		for j := 0; j < m; j++ {
			s := Sample{
				ChunkSeq: seq,
				TrackID:  r.u32(),
				Seq:      r.u32(),
				DTS:      r.u64(),
				Duration: r.u32(),
				Offset:   int64(r.u64()),
				Size:     r.u32(),
				Flags:    r.u32(),
				RTPSeq:   r.u32(),
			}
			samps = append(samps, s)
		}
		chunks = append(chunks, samps)
	}
	if r.err != nil {
		return nil, nil, model.NewError(model.StageDemux, "truncated armd payload").Wrap(r.err)
	}
	return tracks, chunks, nil
}

func kindString(k byte) string {
	switch k {
	case 1:
		return KindVideo
	case 2:
		return KindAudio
	case 3:
		return KindHint
	default:
		return "unknown"
	}
}

// KindByte is the on-disk encoding of a track kind.
func KindByte(kind string) byte {
	switch kind {
	case KindVideo:
		return 1
	case KindAudio:
		return 2
	case KindHint:
		return 3
	default:
		return 0
	}
}

// Open parses the container structure. Sample payloads are streamed later
// with ReadPayload so large mdat regions stay out of memory.
func Open(path string) (*Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.NewError(model.StageDemux, "cannot open input: "+path).Wrap(err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, model.NewError(model.StageDemux, "cannot stat input").Wrap(err)
	}
	c := &Container{Path: path, Size: st.Size(), r: f}

	var armdPayload []byte
	var pendingArmf uint32
	err = scanTopLevel(f, c.Size, func(h *rawHeader) error {
		switch {
		case h.Type == boxTypeFtyp:
			return c.readBrands(f, h)
		case h.Type == boxTypeMoov:
			return c.scanMoov(f, h, &armdPayload)
		case h.Type == boxTypeArmf:
			payload, perr := readBoxPayload(f, h)
			if perr != nil {
				return perr
			}
			if len(payload) < 12 || string(payload[:4]) != armfMagic {
				return model.NewError(model.StageDemux, "bad armf box").WithOffset(h.Offset)
			}
			pendingArmf = u32(payload[4:8])
		case h.Type == boxTypeMdat:
			c.Chunks = append(c.Chunks, Chunk{
				Seq:       pendingArmf,
				DataBegin: h.Offset + int64(h.HeaderSize),
				DataEnd:   h.Offset + int64(h.Size),
				HeaderOff: h.Offset,
			})
			pendingArmf = 0
		}
		return nil
	}, func(h *rawHeader, me *model.Error) error {
		// Truncated final mdat: keep the chunk with data clipped to EOF.
		// Per-sample recovery decisions happen while streaming payloads.
		if h.Type != boxTypeMdat {
			return me
		}
		c.TruncatedAt = h.Offset
		c.Chunks = append(c.Chunks, Chunk{
			Seq:       pendingArmf,
			DataBegin: h.Offset + int64(h.HeaderSize),
			DataEnd:   c.Size,
			HeaderOff: h.Offset,
		})
		c.Truncation = append(c.Truncation, *me)
		return nil
	})
	if err != nil {
		f.Close()
		return nil, err
	}
	if len(armdPayload) == 0 {
		f.Close()
		return nil, model.NewError(model.StageDemux, "missing armd archive metadata box")
	}
	tracks, samples, err := parseArmd(armdPayload)
	if err != nil {
		f.Close()
		return nil, err
	}
	c.Tracks = tracks
	c.Samples = samples
	if err := c.bindChunks(); err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

func (c *Container) readBrands(r io.ReadSeeker, h *rawHeader) error {
	payload, err := readBoxPayload(r, h)
	if err != nil {
		return err
	}
	if len(payload) < 8 {
		return model.NewError(model.StageDemux, "truncated ftyp").WithOffset(h.Offset)
	}
	c.Brands = append(c.Brands, string(payload[0:4]))
	n := int(u32(payload[4:8]))
	for i := 0; i < n && 8+i*4+4 <= len(payload); i++ {
		c.Brands = append(c.Brands, string(payload[8+i*4:12+i*4]))
	}
	for _, b := range c.Brands {
		if b == "qt  " {
			c.IsQuickTime = true
		}
	}
	return nil
}

// scanMoov uses go-mp4 for the standard box hierarchy and extracts our armd
// box through ReadData, so unknown sibling boxes are skipped generically.
func (c *Container) scanMoov(r io.ReadSeeker, h *rawHeader, armd *[]byte) error {
	bi := &mp4.BoxInfo{
		Offset:     uint64(h.Offset),
		Size:       h.Size,
		HeaderSize: h.HeaderSize,
		Type:       h.Type,
	}
	var inner error
	_, err := mp4.ReadBoxStructureFromInternal(r, bi, func(handle *mp4.ReadHandle) (interface{}, error) {
		if handle.BoxInfo.Type == boxTypeArmd {
			var buf bytes.Buffer
			if _, e := handle.ReadData(&buf); e != nil {
				inner = model.NewError(model.StageDemux, "cannot read armd").Wrap(e)
				return nil, inner
			}
			*armd = buf.Bytes()
			return nil, nil
		}
		if len(handle.Path) < 3 {
			return handle.Expand()
		}
		return nil, nil
	})
	if inner != nil {
		return inner
	}
	return err
}

// bindChunks validates that every sample table points inside a known data
// chunk and records per-sample bounds. Fragmented files match armf seq
// numbers; plain files have one mdat and chunk seq 0.
func (c *Container) bindChunks() error {
	bySeq := map[uint32]*Chunk{}
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		bySeq[ch.Seq] = ch
	}
	known := map[uint32]bool{}
	for _, t := range c.Tracks {
		known[t.ID] = true
	}
	for ci, table := range c.Samples {
		var seq uint32
		if ci < len(c.Chunks) {
			seq = c.Chunks[ci].Seq
		}
		ch, ok := bySeq[seq]
		if !ok {
			return model.NewError(model.StageDemux,
				fmt.Sprintf("sample table %d has no matching mdat chunk", ci))
		}
		// Only the final chunk may be clipped by a truncated mdat; earlier
		// chunks keep strict upper bounds.
		clipped := c.TruncatedAt >= 0 && ci == len(c.Samples)-1
		for si := range table {
			s := &table[si]
			if !known[s.TrackID] {
				return model.NewError(model.StageDemux,
					fmt.Sprintf("sample references unknown track %d", s.TrackID)).
					WithTrack(int32(s.TrackID)).WithSample(s.Seq)
			}
			end := s.Offset + int64(s.Size)
			if s.Offset < ch.DataBegin {
				return model.NewError(model.StageDemux,
					fmt.Sprintf("sample offset before mdat: track=%d seq=%d", s.TrackID, s.Seq)).
					WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq)
			}
			if end > ch.DataEnd && !clipped {
				return model.NewError(model.StageDemux,
					fmt.Sprintf("sample offset outside mdat: track=%d seq=%d", s.TrackID, s.Seq)).
					WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq)
			}
		}
		c.Samples[ci] = table
	}
	return nil
}

func readBoxPayload(r io.ReadSeeker, h *rawHeader) ([]byte, error) {
	if _, err := r.Seek(h.Offset+int64(h.HeaderSize), io.SeekStart); err != nil {
		return nil, model.NewError(model.StageDemux, "cannot seek box payload").
			WithOffset(h.Offset).Wrap(err)
	}
	n := h.Size - h.HeaderSize
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, model.NewError(model.StageDemux, "truncated box payload").
			WithOffset(h.Offset).Recover().Wrap(err)
	}
	return buf, nil
}

// Close releases the underlying input file.
func (c *Container) Close() error {
	if cl, ok := c.r.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}
