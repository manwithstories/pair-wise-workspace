// MP4/MOV audio container adapter: entry point and root-level walk.
//
// The adapter parses structure only. Media bytes stay on disk and are read
// on demand through Source.Reader/Cursor; memory is bounded by one sample
// plus the sample tables. Track-level parsing lives in mp4_track.go and the
// sample-table expansion in mp4_table.go.
package container

import (
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediacore/internal/media"
)

// MP4Version is the adapter revision bound into checkpoints.
var MP4Version = Version{Name: "mp4", Major: 1, Minor: 0}

// MP4 is the versioned MP4/MOV audio adapter.
type MP4 struct {
	Opts Options
}

// NewMP4 returns an adapter with the given resource bounds.
func NewMP4(opts Options) *MP4 {
	if opts.MaxMetadataBytes <= 0 {
		opts = DefaultOptions()
	}
	return &MP4{Opts: opts}
}

// Version implements the adapter contract.
func (a *MP4) Version() Version { return MP4Version }

// OpenFile parses path. The caller owns the returned source and file.
func (a *MP4) OpenFile(path string) (*Source, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, media.NewError(media.CodeUsage, -1, 0, "open %s: %v", path, err).Wrap(err)
	}
	src, err := a.Open(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return src, f, nil
}

type parseState struct {
	opts     Options
	src      *Source
	fileSize int64
	info     Info
	mdats    []Range
	moovOff  int64
	seenFtyp bool
	seenMoov bool
}

// Open parses an MP4/MOV from r. r must remain readable for the lifetime of
// the returned Source.
func (a *MP4) Open(r io.ReaderAt) (*Source, error) {
	size, err := sizeOf(r)
	if err != nil {
		return nil, media.NewError(media.CodeUsage, -1, 0, "determine size: %v", err).Wrap(err)
	}
	st := &parseState{
		opts:     a.Opts,
		src:      &Source{r: r, opts: a.Opts, version: MP4Version},
		fileSize: size,
	}
	if err := st.walk(); err != nil {
		return nil, err
	}
	if !st.seenMoov {
		return nil, media.NewError(media.CodeMissingBox, 0, 0, "missing moov box")
	}
	if len(st.src.tracks) == 0 {
		return nil, media.NewError(media.CodeNoAudioTrack, -1, 0, "no audio track in moov")
	}
	st.info.FileSize = size
	st.info.MoovOffset = st.moovOff
	st.info.Mdats = st.mdats
	st.info.TrackCount = len(st.src.tracks)
	st.info.AudioTracks = len(st.src.tracks)
	st.info.DurationMS = st.durationMS()
	st.src.mdats = st.mdats
	st.src.info = st.info
	return st.src, nil
}

func (st *parseState) durationMS() int64 {
	var best int64
	for _, t := range st.src.tracks {
		if t.Timescale > 0 && t.Duration > 0 {
			ms, _, err := media.TimeBase{Timescale: t.Timescale}.Duration(t.Duration, media.TimeBase{Timescale: 1000})
			if err == nil && ms > best {
				best = ms
			}
		}
	}
	return best
}

// walk reads the root box sequence one header at a time. Any length that
// exceeds the file is rejected by readBoxHeader before the walk continues.
func (st *parseState) walk() error {
	off := int64(0)
	first := true
	for off < st.fileSize {
		bi, err := readBoxHeader(st.src.r, off, st.fileSize)
		if err != nil {
			return err
		}
		if first && bi.typ != "ftyp" && bi.typ != "moov" && bi.typ != "mdat" && bi.typ != "free" && bi.typ != "wide" {
			return media.NewError(media.CodeInvalidBoxLength, off, 0,
				"unexpected first box type %q", bi.typ)
		}
		first = false
		switch bi.typ {
		case "ftyp":
			if err := st.parseFtyp(bi); err != nil {
				return err
			}
			st.seenFtyp = true
		case "moov":
			if err := st.parseMoov(bi); err != nil {
				return err
			}
			st.seenMoov = true
			st.moovOff = bi.start
		case "mdat":
			st.mdats = append(st.mdats, Range{Start: bi.payload, End: bi.end})
		}
		if bi.end <= off {
			return media.NewError(media.CodeInvalidBoxLength, off, 0, "box %q does not advance", bi.typ)
		}
		off = bi.end
	}
	return nil
}

func (st *parseState) parseFtyp(bi boxHeader) error {
	if bi.size-bi.headerSize > st.opts.MaxMetadataBytes {
		return media.NewError(media.CodeOversizedBox, bi.start, 0, "ftyp too large")
	}
	body, err := readPayload(st.src.r, bi)
	if err != nil {
		return err
	}
	if len(body) < 8 {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "ftyp payload %d < 8", len(body))
	}
	st.info.MajorBrand = string(body[0:4])
	for i := 8; i+4 <= len(body); i += 4 {
		st.info.Compatible = append(st.info.Compatible, string(body[i:i+4]))
	}
	return nil
}

func (st *parseState) parseMoov(bi boxHeader) error {
	if bi.size-bi.headerSize > st.opts.MaxMetadataBytes {
		return media.NewError(media.CodeOversizedBox, bi.start, 0, "moov too large")
	}
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		if child.typ == "trak" {
			return st.parseTrak(child)
		}
		return nil
	})
}

// decodeBox unmarshals a full box body into dst with go-mp4.
func (st *parseState) decodeBox(bi boxHeader, dst mp4.IBox) error {
	body, err := readPayload(st.src.r, bi)
	if err != nil {
		return err
	}
	if _, err := mp4.Unmarshal(bytesReader(body), uint64(len(body)), dst, mp4.Context{}); err != nil {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "parse %s: %v", bi.typ, err).Wrap(err)
	}
	return nil
}

type trackBuild struct {
	id           int
	trackOffset  int64
	timescale    int64
	duration     int64
	handler      string
	codecTag     string
	rate         int
	channels     int
	bits         int
	config       []byte
	sampleSizes  []uint32
	sampleSize   uint32
	stts         []mp4.SttsEntry
	ctts         []mp4.CttsEntry
	cttsVersion  uint8
	chunkOffsets []uint64
	stsc         []mp4.StscEntry
}

func (st *parseState) parseTrak(bi boxHeader) error {
	tb := &trackBuild{trackOffset: bi.start}
	err := children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		switch child.typ {
		case "tkhd":
			return st.readTkhd(tb, child)
		case "mdia":
			return st.parseMdia(tb, child)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if tb.handler != "soun" {
		return nil
	}
	return st.finishTrack(tb, bi)
}

func (st *parseState) readTkhd(tb *trackBuild, bi boxHeader) error {
	var box mp4.Tkhd
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.id = int(box.TrackID)
	return nil
}

func (st *parseState) parseMdia(tb *trackBuild, bi boxHeader) error {
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		switch child.typ {
		case "mdhd":
			return st.readMdhd(tb, child)
		case "hdlr":
			return st.readHdlr(tb, child)
		case "minf":
			return st.parseMinf(tb, child)
		}
		return nil
	})
}

func (st *parseState) readMdhd(tb *trackBuild, bi boxHeader) error {
	var box mp4.Mdhd
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.timescale = int64(box.Timescale)
	tb.duration = int64(box.GetDuration())
	if tb.timescale <= 0 || tb.timescale > 1<<31-1 {
		return media.NewError(media.CodeUnsupportedTrack, bi.start, 0,
			"invalid mdhd timescale %d", box.Timescale)
	}
	return nil
}

func (st *parseState) readHdlr(tb *trackBuild, bi boxHeader) error {
	var box mp4.Hdlr
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.handler = string(box.HandlerType[:])
	return nil
}

func (st *parseState) parseMinf(tb *trackBuild, bi boxHeader) error {
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		if child.typ == "stbl" {
			return st.parseStbl(tb, child)
		}
		return nil
	})
}

func (st *parseState) parseStbl(tb *trackBuild, bi boxHeader) error {
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		switch child.typ {
		case "stsd":
			return st.readStsd(tb, child)
		case "stts":
			return st.readStts(tb, child)
		case "ctts":
			return st.readCtts(tb, child)
		case "stsz":
			return st.readStsz(tb, child)
		case "stco":
			return st.readStco(tb, child, false)
		case "co64":
			return st.readStco(tb, child, true)
		case "stsc":
			return st.readStsc(tb, child)
		}
		return nil
	})
}

func (st *parseState) readStsd(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stsd
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	// The first sample entry starts after version/flags + entry count.
	entryOff := bi.payload + 8
	if entryOff >= bi.end {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "stsd has no sample entry")
	}
	entry, err := readBoxHeader(st.src.r, entryOff, bi.end)
	if err != nil {
		return err
	}
	tb.codecTag = entry.typ
	entBody, err := readPayload(st.src.r, entry)
	if err != nil {
		return err
	}
	if len(entBody) < 28 {
		return media.NewError(media.CodeInvalidBoxLength, entry.start, 0,
			"audio sample entry too short: %d", len(entBody))
	}
	// AudioSampleEntry after the 8-byte SampleEntry base:
	// reserved[6] data_reference_index(2) version(2) reserved[6]
	// channelcount(2) samplesize(2) pre_defined(2) reserved(2) rate(4).
	tb.channels = int(be16(entBody[16:18]))
	tb.bits = int(be16(entBody[18:20]))
	tb.rate = int(be32(entBody[24:28]) >> 16)
	if tb.rate == 0 {
		tb.rate = int(be32(entBody[24:28]))
	}
	return st.readEntryChildren(tb, entry, entBody)
}

// readEntryChildren walks the boxes (esds, wave, ...) below the sample entry.
func (st *parseState) readEntryChildren(tb *trackBuild, entry boxHeader, body []byte) error {
	bodyLen := int64(28)
	if len(body) >= 10 {
		switch be16(body[8:10]) {
		case 1:
			bodyLen = 44
		case 2:
			bodyLen = 64
		}
	}
	off := entry.payload + bodyLen
	if off > entry.end {
		off = entry.end
	}
	for off+8 <= entry.end {
		child, err := readBoxHeader(st.src.r, off, entry.end)
		if err != nil {
			return err
		}
		if child.typ == "esds" {
			payload, err := readPayload(st.src.r, child)
			if err != nil {
				return err
			}
			if cfg, err := extractASC(payload); err == nil && len(tb.config) == 0 {
				tb.config = cfg
			}
		}
		if child.end <= off {
			break
		}
		off = child.end
	}
	return nil
}

// extractASC scans an esds payload for the DecoderSpecificInfo descriptor
// (tag 0x05) and returns its bytes. Tolerance is deliberate: the codec layer
// validates the ASC and reports a precise error if it is malformed.
func extractASC(esds []byte) ([]byte, error) {
	i := 0
	if len(esds) >= 4 {
		i = 4 // version + flags
	}
	for i < len(esds) {
		tag := esds[i]
		i++
		size, n, ok := readDescLen(esds[i:])
		if !ok {
			return nil, fmt.Errorf("truncated descriptor length at %d", i)
		}
		i += n
		if size < 0 || i+size > len(esds) {
			return nil, fmt.Errorf("descriptor 0x%02x overruns esds", tag)
		}
		if tag == 0x05 {
			out := make([]byte, size)
			copy(out, esds[i:i+size])
			return out, nil
		}
		if tag == 0x03 || tag == 0x04 {
			inner := esds[i : i+size]
			if tag == 0x03 && len(inner) >= 3 {
				skip := 3
				flags := inner[2]
				if flags&0x80 != 0 {
					skip += 2
				}
				if flags&0x40 != 0 && skip < len(inner) {
					skip += 1 + int(inner[skip])
				}
				if flags&0x20 != 0 {
					skip += 2
				}
				if skip < len(inner) {
					inner = inner[skip:]
				} else {
					inner = nil
				}
			}
			if tag == 0x04 && len(inner) >= 13 {
				inner = inner[13:]
			}
			if cfg, err := extractASC(inner); err == nil && len(cfg) > 0 {
				return cfg, nil
			}
		}
		i += size
	}
	return nil, nil
}

func readDescLen(b []byte) (size int, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	v := 0
	for n = 0; n < 4 && n < len(b); n++ {
		v = v<<7 | int(b[n]&0x7f)
		if b[n]&0x80 == 0 {
			return v, n + 1, true
		}
	}
	return 0, 0, false
}
func (st *parseState) readStts(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stts
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.stts = box.Entries
	return nil
}

func (st *parseState) readCtts(tb *trackBuild, bi boxHeader) error {
	var box mp4.Ctts
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.ctts = box.Entries
	tb.cttsVersion = box.GetVersion()
	return nil
}

func (st *parseState) readStsz(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stsz
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	if int(box.SampleCount) > st.opts.MaxSamplesPerTrack {
		return media.NewError(media.CodeMemoryLimit, bi.start, 0,
			"stsz sample count %d above limit", box.SampleCount)
	}
	tb.sampleSize = box.SampleSize
	tb.sampleSizes = box.EntrySize
	return nil
}

func (st *parseState) readStco(tb *trackBuild, bi boxHeader, co64 bool) error {
	body, err := readPayload(st.src.r, bi)
	if err != nil {
		return err
	}
	const n = 8
	entry := 4
	if co64 {
		entry = 8
	}
	if len(body) < n {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "chunk offset table too short")
	}
	count := int(be32(body[4:8]))
	if count > st.opts.MaxChunkCount {
		return media.NewError(media.CodeMemoryLimit, bi.start, 0, "chunk count %d above limit", count)
	}
	if len(body) < n+count*entry {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0,
			"chunk offset table declares %d entries, only %d bytes", count, len(body)-n)
	}
	offs := make([]uint64, count)
	for i := 0; i < count; i++ {
		if co64 {
			offs[i] = be64(body[n+i*8 : n+i*8+8])
		} else {
			offs[i] = uint64(be32(body[n+i*4 : n+i*4+4]))
		}
	}
	tb.chunkOffsets = offs
	return nil
}

func (st *parseState) readStsc(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stsc
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	for _, e := range box.Entries {
		if int(e.FirstChunk) > st.opts.MaxChunkCount {
			return media.NewError(media.CodeMemoryLimit, bi.start, 0,
				"stsc first chunk %d above limit", e.FirstChunk)
		}
	}
	tb.stsc = box.Entries
	return nil
}

// finishTrack expands a completed audio track and appends it to the source.

func (st *parseState) finishTrack(tb *trackBuild, trak boxHeader) error {
	if tb.timescale <= 0 {
		return media.NewError(media.CodeUnsupportedTrack, trak.start, 0,
			"audio track %d has no mdhd timescale", tb.id)
	}
	sampleCount := len(tb.sampleSizes)
	if sampleCount == 0 {
		for _, e := range tb.stts {
			sampleCount += int(e.SampleCount)
		}
	}
	if len(tb.stts) == 0 {
		return media.NewError(media.CodeMissingBox, trak.start, 0, "audio track missing stts")
	}
	if sampleCount > st.opts.MaxSamplesPerTrack {
		return media.NewError(media.CodeMemoryLimit, trak.start, 0,
			"track sample count %d above limit", sampleCount)
	}
	if sampleCount > 0 && (len(tb.chunkOffsets) == 0 || len(tb.stsc) == 0) {
		return media.NewError(media.CodeMissingBox, trak.start, 0, "audio track missing chunk tables")
	}
	number := len(st.src.tracks) + 1
	refs, firstOff, err := buildSampleRefs(tb, sampleCount, st.fileSize)
	if err != nil {
		if me, ok := media.AsError(err); ok {
			me.Track = number
			return me
		}
		return err
	}
	tr := Track{
		Number:            number,
		ID:                tb.id,
		Timescale:         tb.timescale,
		Duration:          tb.duration,
		Handler:           tb.handler,
		SampleCount:       sampleCount,
		CodecTag:          tb.codecTag,
		SampleRate:        tb.rate,
		Channels:          tb.channels,
		BitsPerSample:     tb.bits,
		Config:            tb.config,
		FirstSampleOffset: firstOff,
	}
	st.src.tracks = append(st.src.tracks, tr)
	st.src.refs = append(st.src.refs, refs)
	return nil
}

// buildSampleRefs maps every sample index to an absolute file offset and a
// DTS/PTS in track ticks, validating the chunk map against stsz and against
// the file bounds. It retains O(samples) references but no media bytes.
func buildSampleRefs(tb *trackBuild, sampleCount int, fileSize int64) ([]SampleRef, int64, error) {
	refs := make([]SampleRef, sampleCount)
	var timed uint64
	for _, e := range tb.stts {
		timed += uint64(e.SampleCount)
	}
	if int(timed) < sampleCount {
		return nil, -1, media.NewError(media.CodeInvalidBoxLength, -1, 0,
			"stts covers %d samples, stsz has %d", timed, sampleCount)
	}

	sttsPos, sttsLeft := 0, 0
	if len(tb.stts) > 0 {
		sttsLeft = int(tb.stts[0].SampleCount)
	}
	nextDelta := func() int64 {
		for sttsPos < len(tb.stts) && sttsLeft == 0 {
			sttsPos++
			if sttsPos < len(tb.stts) {
				sttsLeft = int(tb.stts[sttsPos].SampleCount)
			}
		}
		if sttsPos >= len(tb.stts) {
			return 0
		}
		sttsLeft--
		return int64(tb.stts[sttsPos].SampleDelta)
	}

	cttsPos, cttsLeft := 0, 0
	if len(tb.ctts) > 0 {
		cttsLeft = int(tb.ctts[0].SampleCount)
	}
	nextCTTS := func() int64 {
		for cttsPos < len(tb.ctts) && cttsLeft == 0 {
			cttsPos++
			if cttsPos < len(tb.ctts) {
				cttsLeft = int(tb.ctts[cttsPos].SampleCount)
			}
		}
		if cttsPos >= len(tb.ctts) {
			return 0
		}
		cttsLeft--
		if tb.cttsVersion == 1 {
			return int64(tb.ctts[cttsPos].SampleOffsetV1)
		}
		return int64(tb.ctts[cttsPos].SampleOffsetV0)
	}

	// samplesPerChunk resolves the stsc run covering a 1-based chunk index.
	samplesPerChunk := func(chunk int) (uint32, bool) {
		var per uint32
		found := false
		for _, e := range tb.stsc {
			if int(e.FirstChunk) <= chunk {
				per, found = e.SamplesPerChunk, true
			} else {
				break
			}
		}
		return per, found
	}

	sampleIdx := 0
	dts := int64(0)
	var firstOff int64 = -1
	for ci, chunkOff := range tb.chunkOffsets {
		per, ok := samplesPerChunk(ci + 1)
		if !ok {
			return nil, -1, media.NewError(media.CodeInvalidBoxLength, -1, 0,
				"stsc has no entry covering chunk %d", ci+1)
		}
		off := int64(chunkOff)
		for j := uint32(0); j < per && sampleIdx < sampleCount; j++ {
			var size uint32
			if tb.sampleSize != 0 {
				size = tb.sampleSize
			} else {
				size = tb.sampleSizes[sampleIdx]
			}
			delta := nextDelta()
			refs[sampleIdx] = SampleRef{
				Offset:   off,
				Size:     int32(size),
				DTS:      dts,
				Duration: delta,
				CTTS:     nextCTTS(),
			}
			if firstOff < 0 {
				firstOff = off
			}
			if off < 0 || off+int64(size) > fileSize {
				// Attribute to the first byte that should exist but does
				// not: the truncation point when the sample starts inside the
				// file, otherwise the sample's own start.
				bad := off
				if off < fileSize {
					bad = fileSize
				}
				return nil, -1, media.NewError(media.CodeTruncatedMdat, bad, 0,
					"sample %d needs %d bytes at %d, file is %d bytes", sampleIdx, size, off, fileSize)
			}
			off += int64(size)
			dts += delta
			sampleIdx++
		}
		if sampleIdx >= sampleCount {
			break
		}
	}
	if sampleIdx < sampleCount {
		return nil, -1, media.NewError(media.CodeInvalidBoxLength, -1, 0,
			"chunk tables cover %d of %d samples", sampleIdx, sampleCount)
	}
	return refs, firstOff, nil
}
