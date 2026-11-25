package remux

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/abema/go-mp4"

	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
)

// mp4Track accumulates samples for one output track across the current
// fragment.
type mp4Track struct {
	route     mapping.Route
	timescale uint32
	samples   []mp4Sample
}

type mp4Sample struct {
	dts      media.Timestamp
	dur      media.Timestamp
	keyframe bool
	data     []byte
}

// mp4Sink writes fragmented MP4. ftyp, moov with mvex/trex, and per-fragment
// moof+mdat pairs are emitted as packets arrive. Leaf boxes are marshalled with
// go-mp4 so field layouts stay spec-exact; containers are framed here because
// the marshaller does not expand children.
type mp4Sink struct {
	hw     *hasherWriter
	plan   *mapping.Plan
	opts   Options
	tracks []*mp4Track
	byOut  map[uint64]*mp4Track

	fragStart media.Timestamp
	fragOpen  bool
	fragSeq   uint32
	fragments int
	packets   int
	sum       Summary
	closed    bool
	timeStart media.Timestamp
	timeEnd   media.Timestamp
	haveTime  bool
	writeErr  error
}

func newMP4Sink(w io.Writer, plan *mapping.Plan, tracks []media.Track, opts Options) (Sink, error) {
	_ = tracks
	s := &mp4Sink{
		hw:    newHasherWriter(w),
		plan:  plan,
		opts:  opts,
		byOut: make(map[uint64]*mp4Track),
	}
	for _, r := range orderedTracks(plan) {
		ts := uint32(1_000_000)
		if r.OutType == media.TrackAudio {
			ts = 48000
		}
		t := &mp4Track{route: r, timescale: ts}
		s.tracks = append(s.tracks, t)
		s.byOut[r.OutTrack] = t
	}
	if err := s.writeInit(); err != nil {
		return nil, err
	}
	return s, nil
}

// box writes a container or leaf header plus payload.
func (s *mp4Sink) box(typ mp4.BoxType, payload []byte) error {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)+8))
	copy(hdr[4:], typ[:])
	if _, err := s.hw.Write(hdr[:]); err != nil {
		return media.NewError("remux.mp4", media.ClassInterrupted, err)
	}
	if _, err := s.hw.Write(payload); err != nil {
		return media.NewError("remux.mp4", media.ClassInterrupted, err)
	}
	return nil
}

func (s *mp4Sink) writeInit() error {
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISO5(), MinorVersion: 512}
	ftyp.AddCompatibleBrand(mp4.BrandISO5())
	ftyp.AddCompatibleBrand(mp4.BrandISO6())
	payload, err := marshalBox(ftyp)
	if err != nil {
		return err
	}
	if err := s.box(mp4.BoxTypeFtyp(), payload); err != nil {
		return err
	}
	moov, err := s.buildMoov()
	if err != nil {
		return err
	}
	return s.box(mp4.BoxTypeMoov(), moov)
}

func (s *mp4Sink) buildMoov() ([]byte, error) {
	var moov bodyBuf
	rate := int32(0x00010000)
	vol := int16(0x0100)
	mvhd := &mp4.Mvhd{
		Timescale:   1_000_000,
		Rate:        rate,
		Volume:      vol,
		Matrix:      unityMatrix(),
		NextTrackID: uint32(len(s.tracks) + 1),
	}
	if err := moov.marshalChild(mp4.BoxTypeMvhd(), mvhd); err != nil {
		return nil, err
	}
	for _, t := range s.tracks {
		trak, err := s.buildTrak(t)
		if err != nil {
			return nil, err
		}
		moov.container(mp4.BoxTypeTrak(), trak)
	}
	var mvex bodyBuf
	for _, t := range s.tracks {
		trex := &mp4.Trex{
			TrackID:                       uint32(t.route.OutTrack),
			DefaultSampleDescriptionIndex: 1,
		}
		if err := mvex.marshalChild(mp4.BoxTypeTrex(), trex); err != nil {
			return nil, err
		}
	}
	moov.container(mp4.BoxTypeMvex(), mvex.bytes())
	return moov.bytes(), nil
}

func (s *mp4Sink) buildTrak(t *mp4Track) ([]byte, error) {
	var trak bodyBuf
	tkhd := &mp4.Tkhd{
		TrackID: uint32(t.route.OutTrack),
		Matrix:  unityMatrix(),
	}
	if t.route.OutType == media.TrackVideo {
		tkhd.Width = uint32(t.route.In.Width) << 16
		tkhd.Height = uint32(t.route.In.Height) << 16
	} else {
		tkhd.Volume = 0x0100
	}
	if err := trak.marshalChild(mp4.BoxTypeTkhd(), tkhd); err != nil {
		return nil, err
	}
	mdhd := &mp4.Mdhd{Timescale: t.timescale, Language: langCode(t.route.OutLanguage)}
	var mdia bodyBuf
	if err := mdia.marshalChild(mp4.BoxTypeMdhd(), mdhd); err != nil {
		return nil, err
	}
	hdlr := &mp4.Hdlr{HandlerType: t.handlerType(), Name: t.handlerName()}
	if err := mdia.marshalChild(mp4.BoxTypeHdlr(), hdlr); err != nil {
		return nil, err
	}
	minf, err := s.buildMinf(t)
	if err != nil {
		return nil, err
	}
	mdia.container(mp4.BoxTypeMinf(), minf)
	trak.container(mp4.BoxTypeMdia(), mdia.bytes())
	return trak.bytes(), nil
}

func (s *mp4Sink) buildMinf(t *mp4Track) ([]byte, error) {
	var minf bodyBuf
	switch t.route.OutType {
	case media.TrackVideo:
		if err := minf.marshalChild(mp4.BoxTypeVmhd(), &mp4.Vmhd{}); err != nil {
			return nil, err
		}
	case media.TrackAudio:
		if err := minf.marshalChild(mp4.BoxTypeSmhd(), &mp4.Smhd{}); err != nil {
			return nil, err
		}
	}
	var dinf bodyBuf
	var dref bodyBuf
	url := &mp4.Url{}
	url.AddFlag(mp4.UrlSelfContained)
	if err := dref.marshalChild(mp4.BoxTypeUrl(), url); err != nil {
		return nil, err
	}
	drefPayload := append([]byte{0, 0, 0, 1}, dref.bytes()...)
	var drefBox bodyBuf
	drefBox.box(mp4.BoxTypeDref(), drefPayload)
	dinf.container(mp4.BoxTypeDinf(), drefBox.bytes())
	minf.container(mp4.BoxTypeDinf(), dinf.bytes())

	stsd, err := s.buildStsd(t)
	if err != nil {
		return nil, err
	}
	var stbl bodyBuf
	stbl.boxBytes(stsd)
	minf.container(mp4.BoxTypeStbl(), stbl.bytes())
	return minf.bytes(), nil
}

func (s *mp4Sink) buildStsd(t *mp4Track) ([]byte, error) {
	entry, err := s.buildSampleEntry(t)
	if err != nil {
		return nil, err
	}
	// FullBox header: version+flags then entry_count.
	payload := make([]byte, 8, 8+len(entry))
	binary.BigEndian.PutUint32(payload[4:], 1) // entry_count
	payload = append(payload, entry...)
	return framed(mp4.BoxTypeStsd(), payload), nil
}

func (s *mp4Sink) buildSampleEntry(t *mp4Track) ([]byte, error) {
	switch t.route.OutType {
	case media.TrackVideo:
		return s.buildVisualEntry(t)
	case media.TrackAudio:
		return s.buildAudioEntry(t)
	default:
		return nil, media.OnTrack(media.NewError("remux.mp4", media.ClassUnsupported,
			fmt.Errorf("fMP4 cannot carry %s", t.route.OutType)), t.route.In.ID)
	}
}

// buildVisualEntry writes an avc1 sample entry with a nested avcC.
func (s *mp4Sink) buildVisualEntry(t *mp4Track) ([]byte, error) {
	var payload bytes.Buffer
	payload.Write(make([]byte, 6))  // reserved
	writeU16(&payload, 1)           // data_reference_index
	writeU16(&payload, 0)           // pre_defined
	writeU16(&payload, 0)           // reserved
	payload.Write(make([]byte, 12)) // pre_defined[3]
	writeU16(&payload, uint16(t.route.In.Width))
	writeU16(&payload, uint16(t.route.In.Height))
	writeU32(&payload, 0x00480000) // horizresolution 72dpi
	writeU32(&payload, 0x00480000) // vertresolution
	writeU32(&payload, 0)
	writeU16(&payload, 1)           // frame_count
	payload.Write(make([]byte, 32)) // compressorname
	writeU16(&payload, 0x0018)      // depth
	writeU16(&payload, 0xFFFF)      // pre_defined = -1

	avcC, err := s.buildAvcC(t)
	if err != nil {
		return nil, err
	}
	payload.Write(avcC)
	return framed(mp4.BoxTypeAvc1(), payload.Bytes()), nil
}

func (s *mp4Sink) buildAvcC(t *mp4Track) ([]byte, error) {
	priv := t.route.In.CodecPrivate
	var profile, compat, level byte = 0x64, 0x00, 0x28
	var sps, pps [][]byte
	if len(priv) >= 6 {
		profile, compat, level = priv[1], priv[2], priv[3]
		pos := 6
		for i := 0; i < int(priv[5]&0x1F) && pos+2 <= len(priv); i++ {
			n := int(binary.BigEndian.Uint16(priv[pos:]))
			pos += 2
			if pos+n > len(priv) {
				return nil, media.NewError("remux.mp4", media.ClassFatal,
					fmt.Errorf("avcC SPS %d overruns codec private", i))
			}
			sps = append(sps, priv[pos:pos+n])
			pos += n
		}
		if pos < len(priv) {
			count := int(priv[pos])
			pos++
			for i := 0; i < count && pos+2 <= len(priv); i++ {
				n := int(binary.BigEndian.Uint16(priv[pos:]))
				pos += 2
				if pos+n > len(priv) {
					return nil, media.NewError("remux.mp4", media.ClassFatal,
						fmt.Errorf("avcC PPS %d overruns codec private", i))
				}
				pps = append(pps, priv[pos:pos+n])
				pos += n
			}
		}
	}
	var p bytes.Buffer
	p.WriteByte(1) // configurationVersion
	p.WriteByte(profile)
	p.WriteByte(compat)
	p.WriteByte(level)
	p.WriteByte(0xFC | 0x03) // 6 bits reserved + lengthSizeMinusOne=3
	p.WriteByte(0xE0 | byte(len(sps)&0x1F))
	for _, s := range sps {
		writeU16(&p, uint16(len(s)))
		p.Write(s)
	}
	p.WriteByte(byte(len(pps)))
	for _, s := range pps {
		writeU16(&p, uint16(len(s)))
		p.Write(s)
	}
	return framed(mp4.BoxTypeAvcC(), p.Bytes()), nil
}

// buildAudioEntry writes an Opus sample entry with a nested dOps.
func (s *mp4Sink) buildAudioEntry(t *mp4Track) ([]byte, error) {
	var payload bytes.Buffer
	payload.Write(make([]byte, 6)) // reserved
	writeU16(&payload, 1)          // data_reference_index
	payload.Write(make([]byte, 8)) // reserved[2]
	writeU16(&payload, uint16(t.route.In.Channels))
	writeU16(&payload, 16) // sample_size
	writeU16(&payload, 0)  // pre_defined
	writeU16(&payload, 0)  // reserved
	writeU32(&payload, uint32(t.route.In.SampleRate)<<16)

	var dops bytes.Buffer
	dops.WriteByte(0)                              // version
	dops.WriteByte(byte(t.route.In.Channels))      // output channel count
	writeU16(&dops, 312)                           // pre-skip
	writeU32(&dops, uint32(t.route.In.SampleRate)) // input sample rate
	writeU16(&dops, 0)                             // output gain
	dops.WriteByte(0)                              // mapping family
	payload.Write(framed(mp4.BoxTypeDOps(), dops.Bytes()))
	return framed(mp4.BoxTypeOpus(), payload.Bytes()), nil
}

func (t *mp4Track) handlerType() [4]byte {
	switch t.route.OutType {
	case media.TrackVideo:
		return [4]byte{'v', 'i', 'd', 'e'}
	case media.TrackAudio:
		return [4]byte{'s', 'o', 'u', 'n'}
	default:
		return [4]byte{'t', 'e', 'x', 't'}
	}
}

func (t *mp4Track) handlerName() string {
	switch t.route.OutType {
	case media.TrackVideo:
		return "VideoHandler"
	case media.TrackAudio:
		return "SoundHandler"
	default:
		return "TextHandler"
	}
}

// WritePacket buffers a sample for the current fragment.
func (s *mp4Sink) WritePacket(p media.Packet) error {
	if s.closed {
		return media.NewError("remux.mp4", media.ClassFatal, errSinkClosed{})
	}
	route, ok := planTrackByInput(s.plan, p.Track)
	if !ok {
		return media.AtTrack("remux.mp4", -1, p.Track, media.ClassUnsupported,
			"packet for unmapped track %d", p.Track)
	}
	t := s.byOut[route.OutTrack]
	if t == nil {
		return media.AtTrack("remux.mp4", -1, p.Track, media.ClassFatal, "internal: missing mp4 track")
	}
	dur := p.Duration
	if dur <= 0 {
		dur = t.route.In.DefaultDuration
	}
	if dur <= 0 {
		dur = 1
	}
	t.samples = append(t.samples, mp4Sample{
		dts: p.DTS, dur: dur, keyframe: p.Keyframe, data: p.Data,
	})
	s.packets++
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
	if !s.fragOpen {
		s.fragStart = start
		s.fragOpen = true
	} else if start != s.fragStart {
		if err := s.flushFragment(); err != nil {
			return err
		}
		s.fragStart = start
	}
	return nil
}

// flushFragment writes one moof+mdat pair.
func (s *mp4Sink) flushFragment() error {
	ready := make(map[uint64][]mp4Sample, len(s.tracks))
	total := 0
	for _, t := range s.tracks {
		if len(t.samples) == 0 {
			continue
		}
		sort.SliceStable(t.samples, func(i, j int) bool {
			return t.samples[i].dts < t.samples[j].dts
		})
		ready[t.route.OutTrack] = t.samples
		total += len(t.samples)
		t.samples = nil
	}
	if total == 0 {
		return nil
	}

	var mdat bytes.Buffer
	offsets := make(map[uint64]int64, len(ready))
	var running int64
	for _, t := range s.tracks {
		samples := ready[t.route.OutTrack]
		if samples == nil {
			continue
		}
		offsets[t.route.OutTrack] = running
		for _, smp := range samples {
			mdat.Write(smp.data)
			running += int64(len(smp.data))
		}
	}

	s.fragSeq++
	build := func(moofSize int64) ([]byte, error) {
		var moof bodyBuf
		if err := moof.marshalChild(mp4.BoxTypeMfhd(), &mp4.Mfhd{SequenceNumber: s.fragSeq}); err != nil {
			return nil, err
		}
		for _, t := range s.tracks {
			samples := ready[t.route.OutTrack]
			if samples == nil {
				continue
			}
			traf, err := s.buildTraf(t, samples, moofSize+8+offsets[t.route.OutTrack])
			if err != nil {
				return nil, err
			}
			moof.container(mp4.BoxTypeTraf(), traf)
		}
		return moof.bytes(), nil
	}
	// Two passes: the first measures moof length, the second fills exact
	// trun data offsets.
	probe, err := build(0)
	if err != nil {
		return err
	}
	moofSize := int64(len(probe)) + 8
	moof, err := build(moofSize)
	if err != nil {
		return err
	}
	if int64(len(moof))+8 != moofSize {
		return media.NewError("remux.mp4", media.ClassFatal, fmt.Errorf("moof size drift"))
	}
	if err := s.box(mp4.BoxTypeMoof(), moof); err != nil {
		return err
	}
	if err := s.box(mp4.BoxTypeMdat(), mdat.Bytes()); err != nil {
		return err
	}
	s.fragments++
	return nil
}

func (s *mp4Sink) buildTraf(t *mp4Track, samples []mp4Sample, dataOffset int64) ([]byte, error) {
	var traf bodyBuf
	tfhd := &mp4.Tfhd{TrackID: uint32(t.route.OutTrack)}
	tfhd.AddFlag(mp4.TfhdDefaultBaseIsMoof)
	if err := traf.marshalChild(mp4.BoxTypeTfhd(), tfhd); err != nil {
		return nil, err
	}
	tfdt := &mp4.Tfdt{}
	tfdt.SetVersion(1)
	tfdt.BaseMediaDecodeTimeV1 = uint64(media.ToScale(samples[0].dts, t.timescale))
	if err := traf.marshalChild(mp4.BoxTypeTfdt(), tfdt); err != nil {
		return nil, err
	}
	trun := &mp4.Trun{SampleCount: uint32(len(samples)), DataOffset: int32(dataOffset)}
	trun.AddFlag(0x000001) // data-offset-present
	trun.AddFlag(0x000100) // sample-duration-present
	trun.AddFlag(0x000200) // sample-size-present
	trun.AddFlag(0x000400) // sample-flags-present
	for _, smp := range samples {
		flags := uint32(0x00010000) // sample_is_non_sync_sample
		if smp.keyframe {
			flags = 0x02000000
		}
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration: uint32(media.ToScale(smp.dur, t.timescale)),
			SampleSize:     uint32(len(smp.data)),
			SampleFlags:    flags,
		})
	}
	if err := traf.marshalChild(mp4.BoxTypeTrun(), trun); err != nil {
		return nil, err
	}
	return traf.bytes(), nil
}

// Close flushes the final fragment.
func (s *mp4Sink) Close() (Summary, error) {
	if s.closed {
		return s.sum, errSinkClosed{}
	}
	s.closed = true
	if err := s.flushFragment(); err != nil {
		return s.sum, err
	}
	if s.hw.err != nil {
		return s.sum, media.NewError("remux.mp4", media.ClassInterrupted, s.hw.err)
	}
	s.sum = s.hw.Summary()
	s.sum.Packets = s.packets
	s.sum.Clusters = s.fragments
	s.sum.Target = mapping.TargetFMP4
	s.sum.TimeStart = s.timeStart
	s.sum.TimeEnd = s.timeEnd
	for _, t := range s.tracks {
		s.sum.Tracks = append(s.sum.Tracks, t.route.In)
	}
	return s.sum, nil
}

// bodyBuf accumulates child boxes with correct size headers.
type bodyBuf struct{ b bytes.Buffer }

func (body *bodyBuf) bytes() []byte { return body.b.Bytes() }

func (body *bodyBuf) box(typ mp4.BoxType, payload []byte) {
	body.b.Write(framed(typ, payload))
}

func (body *bodyBuf) boxBytes(full []byte) { body.b.Write(full) }

func (body *bodyBuf) container(typ mp4.BoxType, children []byte) {
	body.box(typ, children)
}

func (body *bodyBuf) marshalChild(typ mp4.BoxType, box mp4.IImmutableBox) error {
	payload, err := marshalBox(box)
	if err != nil {
		return err
	}
	body.box(typ, payload)
	return nil
}

// marshalBox marshals a box's payload. mp4.Marshal emits fields only; the
// common box header is framed by bodyBuf, so nothing is stripped here.
func marshalBox(box mp4.IImmutableBox) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, box, mp4.Context{}); err != nil {
		return nil, media.NewError("remux.mp4", media.ClassFatal, err)
	}
	return buf.Bytes(), nil
}

// framed prepends an 8-byte box header to payload.
func framed(typ mp4.BoxType, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	copy(out[4:], typ[:])
	copy(out[8:], payload)
	return out
}

func unityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func writeU16(w *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	w.Write(b[:])
}

func writeU32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

// langCode converts an ISO-639-2 code into mdhd's packed 3x5-bit form.
func langCode(lang string) [3]byte {
	if len(lang) != 3 {
		lang = "und"
	}
	packed := uint16(0)
	for i := 0; i < 3; i++ {
		c := lang[i]
		if c < 'a' || c > 'z' {
			c = ' '
		}
		packed = packed<<5 | uint16(c-0x60)
	}
	return [3]byte{byte(packed >> 8), byte(packed >> 4), byte((packed & 0xF) << 4)}
}

// FragmentSample is one sample recovered from an fMP4 fragment.
type FragmentSample struct {
	TrackID  uint32
	DTS      media.Timestamp
	Duration media.Timestamp
	Keyframe bool
	Data     []byte
}

// Fragments re-reads an fMP4 file written by this package. It walks the root
// boxes with go-mp4, validates each moof/traf/trun, resolves sample offsets
// against the following mdat, and returns every sample in file order. This is
// the verification path for fragmented output, which go-mp4's Probe cannot
// handle because it expects a non-fragmented sample table.
func Fragments(data []byte) ([]FragmentSample, error) {
	reader := bytes.NewReader(data)
	trackTimescale := make(map[uint32]uint32)
	var out []FragmentSample
	var (
		pendingRun []pendingTraf
		mdatStart  int64
		mdatEnd    int64
	)
	for offset := int64(0); offset < int64(len(data)); {
		bi, err := mp4.ReadBoxInfo(reader)
		if err != nil {
			return nil, media.NewError("remux.verify", media.ClassFatal, err)
		}
		payloadSize := bi.Size - bi.HeaderSize
		switch bi.Type {
		case mp4.BoxTypeFtyp():
			// informational only
		case mp4.BoxTypeMoov():
			ts, err := moovTimescales(reader, bi, payloadSize)
			if err != nil {
				return nil, err
			}
			for k, v := range ts {
				trackTimescale[k] = v
			}
		case mp4.BoxTypeMoof():
			runs, err := readMoof(reader, bi, payloadSize)
			if err != nil {
				return nil, err
			}
			for i := range runs {
				runs[i].moofStart = int64(bi.Offset)
			}
			pendingRun = runs
		case mp4.BoxTypeMdat():
			mdatStart = int64(bi.Offset + bi.HeaderSize)
			mdatEnd = mdatStart + int64(payloadSize)
			if len(pendingRun) > 0 {
				samples, err := resolveRuns(reader, data, pendingRun, mdatStart, mdatEnd, trackTimescale)
				if err != nil {
					return nil, err
				}
				out = append(out, samples...)
				pendingRun = nil
			}
		}
		next := bi.Offset + bi.Size
		if next <= uint64(offset) {
			return nil, media.NewError("remux.verify", media.ClassFatal, fmt.Errorf("non-advancing box at %d", offset))
		}
		offset = int64(next)
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			return nil, media.NewError("remux.verify", media.ClassFatal, err)
		}
	}
	if len(pendingRun) > 0 {
		return nil, media.NewError("remux.verify", media.ClassFatal, fmt.Errorf("moof without matching mdat"))
	}
	return out, nil
}

// pendingTraf is one traf's trun decoded but not yet bound to sample bytes.
type pendingTraf struct {
	trackID    uint32
	baseDTS    uint64
	entries    []mp4.TrunEntry
	dataOffset int64
	moofStart  int64
}

func moovTimescales(r io.ReadSeeker, moov *mp4.BoxInfo, payloadSize uint64) (map[uint32]uint32, error) {
	out := make(map[uint32]uint32)
	// Use trailing slashes to mean "descend further" is not supported, so pull
	// each tkhd/mdhd pair explicitly and pair them by declaration order.
	var ids []uint32
	var scales []uint32
	paths := []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeTkhd()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
	}
	if _, err := moov.SeekToPayload(r); err != nil {
		return nil, media.NewError("remux.verify", media.ClassFatal, err)
	}
	_ = payloadSize
	boxes, err := mp4.ExtractBoxesWithPayload(r, nil, paths)
	if err != nil {
		return nil, media.NewError("remux.verify", media.ClassFatal, err)
	}
	for _, b := range boxes {
		switch box := b.Payload.(type) {
		case *mp4.Tkhd:
			ids = append(ids, box.TrackID)
		case *mp4.Mdhd:
			scales = append(scales, box.Timescale)
		}
	}
	if len(ids) != len(scales) {
		return nil, media.NewError("remux.verify", media.ClassFatal,
			fmt.Errorf("trak tkhd/mdhd count mismatch: %d vs %d", len(ids), len(scales)))
	}
	for i, id := range ids {
		out[id] = scales[i]
	}
	return out, nil
}

func readMoof(r io.ReadSeeker, moof *mp4.BoxInfo, payloadSize uint64) ([]pendingTraf, error) {
	paths := []mp4.BoxPath{
		{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTfhd()},
		{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTfdt()},
		{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTrun()},
	}
	boxes, err := mp4.ExtractBoxesWithPayload(r, nil, paths)
	if err != nil {
		return nil, media.NewError("remux.verify", media.ClassFatal, err)
	}
	// ExtractBoxesWithPayload reads every moof in the file; keep only this one.
	limit := moof.Offset + moof.Size
	filtered := boxes[:0]
	for _, b := range boxes {
		if b.Info.Offset >= moof.Offset && b.Info.Offset < limit {
			filtered = append(filtered, b)
		}
	}
	boxes = filtered
	var runs []pendingTraf
	index := make(map[uint32]int)
	add := func(id uint32) *pendingTraf {
		if i, ok := index[id]; ok {
			return &runs[i]
		}
		runs = append(runs, pendingTraf{trackID: id})
		index[id] = len(runs) - 1
		return &runs[len(runs)-1]
	}
	for _, b := range boxes {
		switch box := b.Payload.(type) {
		case *mp4.Tfhd:
			add(box.TrackID)
		}
	}
	// tfhd/tfdt/trun appear in order per traf; track them positionally.
	pos := 0
	for _, b := range boxes {
		switch box := b.Payload.(type) {
		case *mp4.Tfhd:
			if pos >= len(runs) {
				break
			}
			runs[pos].trackID = box.TrackID
		case *mp4.Tfdt:
			if pos < len(runs) {
				runs[pos].baseDTS = box.GetBaseMediaDecodeTime()
			}
		case *mp4.Trun:
			if pos < len(runs) {
				runs[pos].entries = box.Entries
				runs[pos].dataOffset = int64(box.DataOffset)
			}
			pos++
		}
	}
	return runs, nil
}

func resolveRuns(r io.ReadSeeker, data []byte, runs []pendingTraf, mdatStart, mdatEnd int64, scales map[uint32]uint32) ([]FragmentSample, error) {
	var out []FragmentSample
	for _, run := range runs {
		scale := scales[run.trackID]
		if scale == 0 {
			scale = 1_000_000
		}
		// tfhd sets default-base-is-moof, so trun data_offset is relative to
		// the moof start rather than the mdat payload.
		cursor := run.moofStart + run.dataOffset
		dts := media.FromScale(int64(run.baseDTS), scale)
		for _, e := range run.entries {
			size := int64(e.SampleSize)
			if cursor < mdatStart || cursor+size > mdatEnd {
				return nil, media.NewError("remux.verify", media.ClassFatal,
					fmt.Errorf("sample range %d..%d outside mdat %d..%d", cursor, cursor+size, mdatStart, mdatEnd))
			}
			out = append(out, FragmentSample{
				TrackID:  run.trackID,
				DTS:      dts,
				Duration: media.FromScale(int64(e.SampleDuration), scale),
				Keyframe: e.SampleFlags&0x00010000 == 0,
				Data:     append([]byte(nil), data[cursor:cursor+size]...),
			})
			cursor += size
			dts += media.FromScale(int64(e.SampleDuration), scale)
		}
	}
	return out, nil
}
