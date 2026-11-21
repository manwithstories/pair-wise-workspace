package probe

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"mediad/internal/clock"
	"mediad/internal/model"
)

const (
	klvKeySize = 16
	// runInLimit is the largest partition run-in field accepted.
	runInLimit = 64
	// klvLimit caps a single non-partition KLV payload.
	klvLimit = 64 << 20
)

// MXF probes an MXF file written by mediad's fixture writer.
func MXF(path string) (model.Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.Container{}, model.Wrap(model.KindInvalidInput, "probe.mxf", "open", err)
	}
	defer f.Close()
	return MXFReader(f)
}

// MXFReader probes an MXF stream. The reader must be seekable.
func MXFReader(r io.ReadSeeker) (model.Container, error) {
	scan, err := scanMXF(r)
	if err != nil {
		return model.Container{}, err
	}
	return assembleMXF(scan)
}

// mxfScan is the result of the KLV pass.
type mxfScan struct {
	primer    primer
	preface   *LocalSet
	content   *LocalSet
	packages  []*LocalSet
	tracks    []*LocalSet
	descs     []*LocalSet
	sequences map[[32]byte]*LocalSet
	clips     map[[32]byte]*LocalSet
	timecodes map[[32]byte]*LocalSet
	essence   []essenceSpan
}

type essenceSpan struct {
	trackNo uint32
	off     uint64
	length  uint64
}

func scanMXF(r io.ReadSeeker) (*mxfScan, error) {
	s := &mxfScan{
		primer:    defaultPrimer(),
		sequences: map[[32]byte]*LocalSet{},
		clips:     map[[32]byte]*LocalSet{},
		timecodes: map[[32]byte]*LocalSet{},
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var pos uint64
	hdr := make([]byte, klvKeySize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "read KLV key", err)
		}
		var key UL
		copy(key[:], hdr)
		length, nlen, err := readBERFrom(r)
		if err != nil {
			return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "read KLV length", err)
		}
		valOff := pos + klvKeySize + uint64(nlen)
		headerLen := klvKeySize + uint64(nlen)

		switch key {
		case ULHeaderPartition, ULBodyPartition, ULFooterPartition:
			if length > 1<<20 {
				return nil, model.New(model.KindOverLimit, "probe.mxf", "partition pack too large")
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "read partition", err)
			}
			if err := validatePartition(body); err != nil {
				return nil, err
			}
			pos = valOff + length
			continue
		}
		_ = headerLen

		if length > klvLimit {
			return nil, model.New(model.KindOverLimit, "probe.mxf",
				fmt.Sprintf("KLV %s payload %d exceeds 64MiB", key.String(), length))
		}
		val := make([]byte, length)
		if _, err := io.ReadFull(r, val); err != nil {
			return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "truncated KLV value", err)
		}
		pos = valOff + length

		switch key {
		case ULPrimerPack:
			if err := s.parsePrimer(val); err != nil {
				return nil, err
			}
		case ULIndexTable:
			// Index correctness is the index subsystem's concern.
		default:
			if trackNo, ok := trackFromEssenceKey(key); ok && len(s.essence) < 64 {
				s.essence = append(s.essence, essenceSpan{trackNo: trackNo, off: valOff, length: length})
				continue
			}
			set, err := decodeLocalSet(key, valOff, length, val, s.primer)
			if err != nil {
				return nil, err
			}
			s.classify(set)
		}
	}
	if s.preface == nil {
		return nil, model.New(model.KindInvalidMedia, "probe.mxf", "no Preface set")
	}
	if s.content == nil {
		return nil, model.New(model.KindInvalidMedia, "probe.mxf", "no ContentStorage set")
	}
	return s, nil
}

// validatePartition reads the run-in and the local set that follows it.
func validatePartition(body []byte) error {
	if len(body) < 8 {
		return model.New(model.KindInvalidMedia, "probe.mxf", "partition pack too short")
	}
	runIn := binary.BigEndian.Uint32(body[:4])
	if runIn > runInLimit || uint64(runIn)+8 > uint64(len(body)) {
		return model.New(model.KindInvalidMedia, "probe.mxf", "invalid partition run-in")
	}
	// The partition's own local set starts after the run-in field.
	if _, err := decodeLocalSet(ULBodyPartition, 0, 0, body[4+runIn:], defaultPrimer()); err != nil {
		return err
	}
	return nil
}

func readBERFrom(r io.Reader) (uint64, int, error) {
	var one [1]byte
	if _, err := io.ReadFull(r, one[:]); err != nil {
		return 0, 0, err
	}
	if one[0] < 0x80 {
		return uint64(one[0]), 1, nil
	}
	n := int(one[0] & 0x7f)
	if n == 0 || n > 8 {
		return 0, 0, fmt.Errorf("invalid BER length prefix 0x%02x", one[0])
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, 0, err
	}
	var v uint64
	for _, c := range buf {
		v = v<<8 | uint64(c)
	}
	return v, 1 + n, nil
}

// parsePrimer reads a primer pack: a u32 entry count then (tag, UL) pairs.
func (s *mxfScan) parsePrimer(val []byte) error {
	if len(val) < 4 {
		return model.New(model.KindInvalidMedia, "probe.mxf", "primer pack too short")
	}
	count := int(binary.BigEndian.Uint32(val))
	if count < 0 || 4+count*18 != len(val) {
		return model.New(model.KindInvalidMedia, "probe.mxf",
			fmt.Sprintf("primer declares %d entries but payload is %d bytes", count, len(val)))
	}
	p := make(primer, count)
	for i := 0; i < count; i++ {
		e := val[4+i*18:]
		tag := binary.BigEndian.Uint16(e)
		prop, ok := lookupTag(tag)
		if !ok {
			return model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("primer tag 0x%04x is not in the profile", tag))
		}
		var ul UL
		copy(ul[:], e[2:18])
		if !prop.UL.RoundTrips(ul) {
			return model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("primer tag 0x%04x maps to %s, expected %s (tag collision)", tag, ul.String(), prop.UL.String()))
		}
		p[tag] = prop
	}
	s.primer = p
	return nil
}

func (s *mxfScan) classify(set *LocalSet) {
	switch set.Key {
	case ULPreface:
		s.preface = set
	case ULContentStorage:
		s.content = set
	case ULSourcePackage:
		s.packages = append(s.packages, set)
	case ULTimelineTrack:
		s.tracks = append(s.tracks, set)
	case ULSequence:
		if id, ok := set.UMID(); ok {
			s.sequences[id] = set
		}
	case ULSourceClip:
		if id, ok := set.UMID(); ok {
			s.clips[id] = set
		}
	case ULTimecodeComponent:
		if id, ok := set.UMID(); ok {
			s.timecodes[id] = set
		}
	case ULCDCIDescriptor, ULWaveDescriptor:
		s.descs = append(s.descs, set)
	}
}

// RoundTrips reports whether u is the property's UL, tolerating files that
// use the ISO "local set" variant whose bytes 8-15 differ only in version.
func (u UL) RoundTrips(other UL) bool {
	if u == other {
		return true
	}
	// Compare the identifying first 8 bytes and the registry-designator tail.
	for i := 0; i < 8; i++ {
		if u[i] != other[i] {
			return false
		}
	}
	for i := 12; i < 16; i++ {
		if u[i] != other[i] {
			return false
		}
	}
	return true
}

// assembleMXF joins the scanned sets into model tracks.
func assembleMXF(s *mxfScan) (model.Container, error) {
	out := model.Container{Format: "mxf"}
	if len(s.tracks) == 0 {
		return out, model.New(model.KindInvalidMedia, "probe.mxf", "no timeline tracks")
	}
	descByUMID := indexByUMID(s.descs)
	clipByUMID := indexByUMID(mapValues(s.clips))
	tcByUMID := indexByUMID(mapValues(s.timecodes))

	for _, t := range s.tracks {
		tr, err := assembleTrack(t, descByUMID, clipByUMID, tcByUMID)
		if err != nil {
			return out, err
		}
		tr.Source = "mxf"
		out.Tracks = append(out.Tracks, tr)
	}
	if err := attachEssence(&out, s); err != nil {
		return out, err
	}
	return out, nil
}

func mapValues(m map[[32]byte]*LocalSet) []*LocalSet {
	out := make([]*LocalSet, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func indexByUMID(sets []*LocalSet) map[[32]byte]*LocalSet {
	out := make(map[[32]byte]*LocalSet, len(sets))
	for _, s := range sets {
		if id, ok := s.UMID(); ok {
			out[id] = s
		}
	}
	return out
}

func assembleTrack(t *LocalSet, descs, clips, timecodes map[[32]byte]*LocalSet) (model.Track, error) {
	var tr model.Track
	id, ok := t.Uint32("TrackID")
	if !ok {
		return tr, model.New(model.KindInvalidMedia, "probe.mxf", "timeline track without TrackID")
	}
	tr.ID = id
	rn, rd, ok := t.Rational("EditRate")
	if !ok || rd <= 0 || rn <= 0 {
		return tr, model.New(model.KindInvalidMedia, "probe.mxf", "track has invalid EditRate").WithTrack(id)
	}
	var err error
	if tr.FrameRate, err = clock.New(int64(rn), int64(rd)); err != nil {
		return tr, model.Wrap(model.KindInvalidMedia, "probe.mxf", "EditRate", err).WithTrack(id)
	}
	// MXF edit units are the media clock; round the edit rate so a
	// 30000/1001 track reports 30 ticks per second while FrameRate keeps the
	// exact fraction.
	rounded := (int64(rn) + int64(rd)/2) / int64(rd)
	if rounded <= 0 {
		rounded = int64(rn)
	}
	tr.Timescale = uint32(rounded)

	units, ok := t.Uint64("Duration")
	if !ok {
		return tr, model.New(model.KindInvalidMedia, "probe.mxf", "track has no Duration").WithTrack(id)
	}
	if tr.Duration, err = editUnits(units, int64(rn), int64(rd)); err != nil {
		return tr, model.Wrap(model.KindInvalidMedia, "probe.mxf", "duration", err).WithTrack(id)
	}
	if sp, ok := t.Uint64("StartPosition"); ok {
		if tr.Start, err = editUnits(sp, int64(rn), int64(rd)); err != nil {
			return tr, model.Wrap(model.KindInvalidMedia, "probe.mxf", "start position", err).WithTrack(id)
		}
		tr.EditOffset = tr.Start
	}

	tr.Kind = kindFromDataDefinition(t)
	if ref, ok := t.StrongRef("DescriptorRef"); ok {
		if d, ok := descs[ref.UMID]; ok {
			applyDescriptor(&tr, d)
		}
	}
	if ref, ok := t.StrongRef("TimecodeRef"); ok {
		if tc, ok := timecodes[ref.UMID]; ok {
			// A rounded timecode base is only authoritative for integral edit
			// rates; for 30000/1001 it would truncate to 30 and silently
			// shorten the track.
			if base, ok := tc.Uint32("RoundedTimecodeBase"); ok && tr.FrameRate.Den == 1 && uint32(tr.FrameRate.Num) == base {
				tr.FrameRate = clock.FromInt(int64(base))
			}
		}
	}
	tr.Codec = codecFor(tr.Kind)
	tr.SampleAspect = clock.FromInt(1)
	if err := tr.Validate(); err != nil {
		return tr, err
	}
	return tr, nil
}

// editUnits converts a count of edit units into seconds at num/den fps.
func editUnits(n uint64, num, den int64) (clock.Rational, error) {
	if n > 1<<40 {
		return clock.Rational{}, fmt.Errorf("%d edit units is implausible", n)
	}
	return clock.New(int64(n)*den, num)
}

func kindFromDataDefinition(t *LocalSet) model.TrackKind {
	if ul, ok := t.ULProperty("DataDefinition"); ok {
		switch ul {
		case ULDataDefPicture:
			return model.Video
		case ULDataDefSound:
			return model.Audio
		}
	}
	return model.Data
}

func codecFor(k model.TrackKind) string {
	switch k {
	case model.Video:
		return "mxf-cdci"
	case model.Audio:
		return "mxf-pcm"
	default:
		return "mxf-data"
	}
}

func applyDescriptor(tr *model.Track, d *LocalSet) {
	cands := tr.ColorCandidates
	switch d.Key {
	case ULCDCIDescriptor:
		if w, ok := d.Uint32("StoredWidth"); ok {
			tr.CodedWidth = w
		}
		if h, ok := d.Uint32("StoredHeight"); ok {
			tr.CodedHeight = h
		}
		if rot, ok := d.Int32("RotationDegrees"); ok {
			tr.Rotation = int(rot)
		}
		if hc, ok := d.Uint32("HorizCrop"); ok {
			tr.Crop.Left, tr.Crop.Right = hc, hc
		}
		if vc, ok := d.Uint32("VertCrop"); ok {
			tr.Crop.Top, tr.Crop.Bottom = vc, vc
		}
		if n, den, ok := d.Rational("AspectRatio"); ok && den != 0 && n > 0 {
			if r, err := clock.New(int64(n), int64(den)); err == nil {
				tr.SampleAspect = r
			}
		}
		prim, okp := d.Uint32("ColorPrimaries")
		tc, okt := d.Uint32("TransferCharacteristic")
		mat, okm := d.Uint32("MatrixCoefficient")
		if okp || okt || okm {
			c := model.ColorSpace{Primaries: uint16(prim), Transfer: uint16(tc), Matrix: uint16(mat), Description: "mxf-ul"}
			if fr, ok := d.Uint32("FullRangeFlag"); ok {
				c.FullRange = fr != 0
			}
			cands = append(cands, model.ColorCandidate{Layer: model.ColorLayerTrack, Color: c, Origin: "descriptor"})
		} else {
			cands = append(cands, model.ColorCandidate{Layer: model.ColorLayerContainer, Color: model.DefaultColor(), Origin: "descriptor/absent"})
		}
	case ULWaveDescriptor:
		if sr, den, ok := d.Rational("AudioSamplingRate"); ok && den != 0 && sr > 0 {
			tr.Timescale = uint32(sr / den)
			tr.FrameRate = clock.FromInt(int64(sr / den))
		}
		cands = append(cands, model.ColorCandidate{Layer: model.ColorLayerContainer, Color: model.DefaultColor(), Origin: "audio/absent"})
	}
	tr.ColorCandidates = cands
}

// attachEssence pairs each essence element with a track by track number and
// materialises one sample per edit unit.
func attachEssence(c *model.Container, s *mxfScan) error {
	byTrack := make(map[uint32]essenceSpan, len(s.essence))
	for _, e := range s.essence {
		byTrack[e.trackNo] = e
	}
	for i := range c.Tracks {
		tr := &c.Tracks[i]
		if sp, ok := byTrack[tr.ID]; ok {
			if err := materialiseEssence(tr, sp); err != nil {
				return err
			}
		}
	}
	return nil
}

func materialiseEssence(tr *model.Track, sp essenceSpan) error {
	if tr.Duration.IsZero() || tr.FrameRate.IsZero() {
		return nil
	}
	units, err := tr.Duration.Mul(tr.FrameRate)
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mxf", "edit unit count", err).WithTrack(tr.ID)
	}
	n := units.Round()
	if n <= 0 {
		return nil
	}
	if uint64(n) > sp.length {
		return model.New(model.KindInvalidMedia, "probe.mxf",
			fmt.Sprintf("essence element of %d bytes cannot hold %d edit units", sp.length, n)).WithTrack(tr.ID)
	}
	// The element's payload is a whole number of edit units; any tail bytes
	// are padding and are reported rather than folded into a sample.
	per := sp.length / uint64(n)
	frameDur, err := clock.FromInt(1).Div(tr.FrameRate)
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mxf", "edit unit duration", err).WithTrack(tr.ID)
	}
	tr.Samples = make([]model.Sample, n)
	for i := int64(0); i < n; i++ {
		off, err := frameDur.MulInt(i)
		if err != nil {
			return model.Wrap(model.KindInvalidMedia, "probe.mxf", "sample offset", err).WithTrack(tr.ID)
		}
		dts, err := tr.Start.Add(off)
		if err != nil {
			return model.Wrap(model.KindInvalidMedia, "probe.mxf", "sample dts", err).WithTrack(tr.ID)
		}
		tr.Samples[i] = model.Sample{
			Index: int(i), Offset: int64(sp.off + uint64(i)*per), Size: uint32(per),
			DTS: dts, PTS: dts, Duration: frameDur,
			// Intra-only CDCI: every edit unit is a sync unit.
			Keyframe: true,
		}
	}
	tr.DeclaredBitrateKbps = float64(uint64(n)*per*8) / 1000 / tr.Duration.Float64()
	return nil
}
