package remux

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"sort"

	"mediapipe/internal/demux"

	"mediapipe/internal/ebml"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
)

// mkvSink writes Matroska/WebM. Packets are buffered until a cluster span is
// complete, then one Cluster element is emitted. The segment header is written
// before the first cluster, so a truncated output is detectable by re-demux.
type mkvSink struct {
	hw   *hasherWriter
	plan *mapping.Plan
	opts Options
	buf  bytes.Buffer
	hdr  *ebml.Writer
	// routes maps input track number to its output route.
	routes map[uint64]mapping.Route
	// output tracks in output-number order.
	outTracks []mapping.Route
	// cluster state
	curCluster int64 // start ns of the open cluster, -1 when none
	pending    []media.Packet
	packets    int
	clusters   int
	sum        Summary
	closed     bool
	// time range
	timeStart, timeEnd media.Timestamp
	haveTime           bool
}

func newMKVSink(w io.Writer, plan *mapping.Plan, tracks []media.Track, opts Options) (Sink, error) {
	routes := make(map[uint64]mapping.Route, len(plan.Kept))
	for _, r := range plan.Kept {
		routes[r.In.ID] = r
	}
	s := &mkvSink{
		hw:         newHasherWriter(w),
		plan:       plan,
		opts:       opts,
		routes:     routes,
		outTracks:  orderedTracks(plan),
		curCluster: -1,
	}
	s.hdr = ebml.NewWriter(s.hw, nil)
	if opts.Resume != nil {
		if err := s.restore(opts.Resume); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err := s.writeHeader(); err != nil {
		return nil, err
	}
	return s, nil
}

// writeHeader emits EBML, Info, and Tracks. Segment is opened with an unknown
// size so packets can stream without a seek-back.
func (s *mkvSink) writeHeader() error {
	if err := s.writeEBMLHeader(); err != nil {
		return err
	}
	if err := s.hdr.WriteID(ebml.IDSegment); err != nil {
		return err
	}
	if s.opts.UnknownSegment {
		if err := s.hdr.WriteUnknownSize(); err != nil {
			return err
		}
	} else {
		// A seek-back to patch a known size is not possible on a plain
		// io.Writer, so a known-size segment is written as unknown here too.
		// Callers that need a seekable segment use WriteToFile.
		if err := s.hdr.WriteUnknownSize(); err != nil {
			return err
		}
	}
	if err := s.writeInfo(); err != nil {
		return err
	}
	if err := s.writeTracks(); err != nil {
		return err
	}
	return nil
}

func (s *mkvSink) writeEBMLHeader() error {
	var p bytes.Buffer
	w := ebml.NewWriter(&p, nil)
	w.Uint(ebml.IDEBMLVersion, 1)
	w.Uint(ebml.IDEBMLReadVersion, 1)
	w.Uint(ebml.IDEBMLMaxIDLength, 4)
	w.Uint(ebml.IDEBMLMaxSizeLength, 8)
	doc := "matroska"
	if s.opts.Target == mapping.TargetWebM {
		doc = "webm"
	}
	w.String(ebml.IDDocType, doc)
	w.Uint(ebml.IDDocTypeVersion, 4)
	return s.hdr.Element(ebml.IDEBML, p.Bytes())
}

func (s *mkvSink) writeInfo() error {
	var p bytes.Buffer
	w := ebml.NewWriter(&p, nil)
	w.Uint(ebml.IDTimestampScale, 1_000_000)
	w.String(ebml.IDMuxingApp, "mediapipe")
	w.String(ebml.IDWritingApp, "mediapipe")
	return s.hdr.Element(ebml.IDInfo, p.Bytes())
}

func (s *mkvSink) writeTracks() error {
	var p bytes.Buffer
	w := ebml.NewWriter(&p, nil)
	for _, r := range s.outTracks {
		var e bytes.Buffer
		ew := ebml.NewWriter(&e, nil)
		ew.Uint(ebml.IDTrackNumber, r.OutTrack)
		ew.Uint(ebml.IDTrackUID, r.In.UID)
		ew.Uint(ebml.IDTrackType, uint64(r.OutType))
		ew.Uint(ebml.IDFlagLacing, 0)
		if r.In.DefaultDuration > 0 {
			ew.Uint(ebml.IDDefaultDuration, uint64(r.In.DefaultDuration))
		}
		lang := r.OutLanguage
		if lang == "" {
			lang = "und"
		}
		ew.String(ebml.IDLanguage, lang)
		ew.String(ebml.IDCodecID, string(r.OutCodec))
		if len(r.In.CodecPrivate) > 0 {
			ew.Bytes(ebml.IDCodecPrivate, r.In.CodecPrivate)
		}
		switch r.OutType {
		case media.TrackVideo:
			var v bytes.Buffer
			vw := ebml.NewWriter(&v, nil)
			vw.Uint(ebml.IDPixelWidth, uint64(r.In.Width))
			vw.Uint(ebml.IDPixelHeight, uint64(r.In.Height))
			ew.Element(ebml.IDVideo, v.Bytes())
		case media.TrackAudio:
			var a bytes.Buffer
			aw := ebml.NewWriter(&a, nil)
			aw.Uint(ebml.IDChannels, uint64(r.In.Channels))
			aw.Float(ebml.IDSamplingFrequency, float64(r.In.SampleRate), 4)
			ew.Element(ebml.IDAudio, a.Bytes())
		}
		if err := w.Element(ebml.IDTrackEntry, e.Bytes()); err != nil {
			return err
		}
	}
	return s.hdr.Element(ebml.IDTracks, p.Bytes())
}

// WritePacket buffers a packet and flushes any cluster whose span is complete.
func (s *mkvSink) WritePacket(p media.Packet) error {
	if s.closed {
		return media.NewError("remux.mkv", media.ClassFatal, errSinkClosed{})
	}
	route, ok := s.routes[p.Track]
	if !ok {
		return media.AtTrack("remux.mkv", -1, p.Track, media.ClassUnsupported,
			"packet for unmapped track %d", p.Track)
	}
	if p.DTS < 0 {
		return media.AtTrack("remux.mkv", -1, p.Track, media.ClassFatal,
			"negative packet timestamp")
	}
	out := p.Clone()
	out.Track = route.OutTrack
	if !s.haveTime {
		s.timeStart, s.timeEnd = p.DTS, p.End()
		s.haveTime = true
	} else {
		if p.DTS < s.timeStart {
			s.timeStart = p.DTS
		}
		if p.End() > s.timeEnd {
			s.timeEnd = p.End()
		}
	}
	span := s.opts.clusterSpan()
	start := media.QuantizeDown(p.DTS, span)
	if s.curCluster < 0 {
		s.curCluster = int64(start)
	}
	if int64(start) != s.curCluster {
		if err := s.flushCluster(); err != nil {
			return err
		}
		s.curCluster = int64(start)
	}
	s.pending = append(s.pending, out)
	s.packets++
	return nil
}

// flushCluster emits the pending packets as one Cluster.
func (s *mkvSink) flushCluster() error {
	if len(s.pending) == 0 {
		if s.curCluster >= 0 {
			s.curCluster = -1
		}
		return nil
	}
	items := append([]media.Packet(nil), s.pending...)
	s.pending = s.pending[:0]
	// Within a cluster, order blocks by (DTS, output track) so the output is
	// byte-identical regardless of the order the pipeline delivered them.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].DTS != items[j].DTS {
			return items[i].DTS < items[j].DTS
		}
		return items[i].Track < items[j].Track
	})
	var body bytes.Buffer
	bw := ebml.NewWriter(&body, nil)
	clusterStart := s.curCluster
	if clusterStart < 0 {
		clusterStart = int64(media.QuantizeDown(items[0].DTS, s.opts.clusterSpan()))
	}
	if err := bw.Uint(ebml.IDTimestamp, uint64(clusterStart/int64(media.Millisecond))); err != nil {
		return err
	}
	for _, p := range items {
		if err := writeSimpleBlock(bw, int64(clusterStart), p); err != nil {
			return err
		}
	}
	if s.opts.ClusterCRC {
		var withCRC bytes.Buffer
		cw := ebml.NewWriter(&withCRC, nil)
		cw.CRC32Element(crc32.ChecksumIEEE(body.Bytes()))
		withCRC.Write(body.Bytes())
		if err := s.emitCluster(withCRC.Bytes()); err != nil {
			return err
		}
	} else {
		if err := s.emitCluster(body.Bytes()); err != nil {
			return err
		}
	}
	s.clusters++
	return nil
}

func (s *mkvSink) emitCluster(payload []byte) error {
	if s.opts.UnknownCluster {
		if err := s.hdr.WriteID(ebml.IDCluster); err != nil {
			return err
		}
		if err := s.hdr.WriteUnknownSize(); err != nil {
			return err
		}
		return s.hdr.WriteRaw(payload)
	}
	return s.hdr.Element(ebml.IDCluster, payload)
}

func writeSimpleBlock(w *ebml.Writer, clusterStart int64, p media.Packet) error {
	var payload bytes.Buffer
	trackVint, err := ebml.EncodeSize(int64(p.Track))
	if err != nil {
		return media.AtTrack("remux.mkv", -1, p.Track, media.ClassFatal, "track number too large")
	}
	payload.Write(trackVint)
	rel := int16((int64(p.DTS) - clusterStart) / int64(media.Millisecond))
	var relBuf [2]byte
	binary.BigEndian.PutUint16(relBuf[:], uint16(rel))
	payload.Write(relBuf[:])
	if p.Keyframe {
		payload.WriteByte(0x80)
	} else {
		payload.WriteByte(0x00)
	}
	payload.Write(p.Data)
	return w.Element(ebml.IDSimpleBlock, payload.Bytes())
}

// restore resumes from a checkpoint: it adopts the running byte count and
// SHA-256 state and skips re-emitting the container header.
func (s *mkvSink) restore(state *ResumeState) error {
	if err := s.hw.restore(state); err != nil {
		return err
	}
	s.packets = state.Packets
	s.clusters = state.Clusters
	s.timeStart = state.TimeStart
	s.timeEnd = state.TimeEnd
	s.haveTime = state.HaveTime
	s.curCluster = -1
	return nil
}

// Flush returns a checkpoint at the last completed cluster boundary. Packets
// belonging to the still-open cluster are not written; a resumed run re-reads
// them from the source so fragment grouping matches an uninterrupted run.
func (s *mkvSink) Flush() (Progress, error) {
	if s.closed {
		return Progress{}, media.NewError("remux.mkv", media.ClassFatal, errSinkClosed{})
	}
	if s.hw.err != nil {
		return Progress{}, media.NewError("remux.mkv", media.ClassInterrupted, s.hw.err)
	}
	state, err := s.hw.state()
	if err != nil {
		return Progress{}, media.NewError("remux.mkv", media.ClassInterrupted, err)
	}
	return Progress{
		Bytes:     s.hw.n,
		Packets:   s.packets - len(s.pending),
		Clusters:  s.clusters,
		HashState: state,
		TimeStart: s.timeStart,
		TimeEnd:   s.timeEnd,
		HaveTime:  s.haveTime,
	}, nil
}

// Close flushes the final cluster and returns the digest of the whole file.
func (s *mkvSink) Close() (Summary, error) {
	if s.closed {
		return s.sum, errSinkClosed{}
	}
	s.closed = true
	if err := s.flushCluster(); err != nil {
		return s.sum, err
	}
	if err := s.hw.err; err != nil {
		return s.sum, media.NewError("remux.mkv", media.ClassInterrupted, err)
	}
	s.sum = s.hw.Summary()
	s.sum.Packets = s.packets
	s.sum.Clusters = s.clusters
	s.sum.Target = s.opts.Target
	s.sum.TimeStart = s.timeStart
	s.sum.TimeEnd = s.timeEnd
	for _, r := range s.outTracks {
		s.sum.Tracks = append(s.sum.Tracks, r.In)
	}
	return s.sum, nil
}

type errSinkClosed struct{}

func (errSinkClosed) Error() string { return "sink already closed" }

// VerifySinkOutput re-demuxes a written Matroska/WebM file and compares every
// payload with the source packet it came from. Because the writer copies bytes
// verbatim, a mismatch means either the writer or the muxer corrupted media.
// The key is (output track, DTS); both sides are sorted before comparison.
func VerifySinkOutput(data []byte, source []media.Packet, plan *mapping.Plan) (VerifyResult, error) {
	res := VerifyResult{}
	written, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil && len(written.Packets) == 0 {
		return res, err
	}
	type key struct {
		track uint64
		dts   media.Timestamp
	}
	want := make(map[key][]uint32, len(source))
	for _, p := range source {
		out, ok := plan.Remap(p.Track)
		if !ok {
			continue
		}
		k := key{out, p.DTS}
		want[k] = append(want[k], crc32.ChecksumIEEE(p.Data))
	}
	res.SourcePackets = len(source)
	res.OutputPackets = len(written.Packets)
	if checked, bad, verr := VerifyCRCs(data, true); verr == nil {
		_ = checked
		res.CRCErrors = bad
	}
	matched := 0
	for _, p := range written.Packets {
		k := key{p.Track, p.DTS}
		list, ok := want[k]
		if !ok || len(list) == 0 {
			res.Mismatches++
			continue
		}
		got := crc32.ChecksumIEEE(p.Data)
		if got != list[0] {
			res.Mismatches++
			continue
		}
		want[k] = list[1:]
		matched++
	}
	res.Matched = matched
	// Any source packet not consumed means the writer dropped data.
	for _, list := range want {
		res.Mismatches += len(list)
	}
	if err != nil && len(written.Packets) > 0 {
		return res, err
	}
	return res, nil
}

// VerifyResult summarises a re-demux comparison.
type VerifyResult struct {
	SourcePackets int
	OutputPackets int
	Matched       int
	Mismatches    int
	CRCErrors     int
}

// VerifyCRCs recomputes every EBML CRC-32 element in a written Matroska file.
// It returns the number of mismatches; a cluster without a CRC element counts
// as a mismatch when requireCRC is true.
func VerifyCRCs(data []byte, requireCRC bool) (checked, bad int, err error) {
	res, derr := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if derr != nil && len(res.Packets) == 0 {
		return 0, 0, derr
	}
	for _, c := range res.Clusters {
		if !c.HasCRC {
			if requireCRC {
				bad++
			}
			continue
		}
		checked++
		if !c.VerifyClusterCRC(data) {
			bad++
		}
	}
	if derr != nil {
		return checked, bad, derr
	}
	return checked, bad, nil
}
