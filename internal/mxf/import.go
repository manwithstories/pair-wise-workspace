package mxf

import (
	"crypto/sha256"

	"mediasync/internal/model"
)

// Known set ULs the reader interprets. Anything else is retained as a warning,
// never guessed at.
var (
	setIdentification  = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x30, 0x00}
	setMaterialPackage = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x36, 0x00}
	setSourcePackage   = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x37, 0x00}
	setTrack           = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x03, 0x01, 0x00}
	setSequence        = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x0f, 0x01, 0x00}
	setCDCI            = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x28, 0x01, 0x00}
	setWave            = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x48, 0x01, 0x00}
	setSourceClip      = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x12, 0x01, 0x00}
	setEssenceMap      = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x7f, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x7f, 0x01, 0x00}
)

// Local tags interpreted by this reader. Unknown tags are ignored, but the
// primer still records their ULs.
const (
	tagTrackID        = uint16(0x4801)
	tagEditRate       = uint16(0x4b01)
	tagSequenceSet    = uint16(0x4803)
	tagDuration       = uint16(0x0202)
	tagEventCount     = uint16(0x0501)
	tagStoredWidth    = uint16(0x3203)
	tagStoredHeight   = uint16(0x3204)
	tagDisplayWidth   = uint16(0x3208)
	tagDisplayHeight  = uint16(0x3209)
	tagFrameLayout    = uint16(0x3202)
	tagAudioSampling  = uint16(0x3d03)
	tagChannelCount   = uint16(0x3d07)
	tagMapEntries     = uint16(0x7f01)
	tagMapTrackID     = uint16(0x7f02)
	tagMapKind        = uint16(0x7f03)
	tagMapFirstPacket = uint16(0x7f04)
	tagMapPacketCount = uint16(0x7f05)
	tagMapEditRate    = uint16(0x7f06)
)

// essenceEntry is one track's placement in the essence packet stream.
type essenceEntry struct {
	trackID     uint32
	kind        model.TrackKind
	firstPacket int
	packetCount int
	rate        editRate
}

// Read parses the stream end to end and returns imported tracks.
func (r *Reader) Read() (*Input, error) {
	in := &Input{}
	packets, parts, err := r.ParseKLV()
	in.Partitions = parts
	if err != nil {
		return in, err
	}
	var primer primerMap
	for _, k := range packets {
		if k.Key != primerUL {
			continue
		}
		primer, err = parsePrimer(k.Value)
		if err != nil {
			return in, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "primer: %v", err)
		}
		break
	}
	if primer == nil {
		return in, model.NewError(model.ErrInvalidKLV, 0, 0, "no primer pack found in header partition")
	}

	var entries []essenceEntry
	for _, k := range packets {
		switch k.Key {
		case setEssenceMap:
			e, err := parseEssenceMap(primer, k)
			if err != nil {
				return in, err
			}
			entries = append(entries, e...)
		}
	}
	if len(entries) == 0 {
		return in, model.NewError(model.ErrInvalidKLV, 0, 0, "no essence map found; cannot place samples")
	}

	essencePackets := make([]KLV, 0, len(packets))
	for _, k := range packets {
		if k.Key == essenceUL {
			essencePackets = append(essencePackets, k)
		}
	}

	for _, e := range entries {
		tr, stats, err := buildTrack(e, essencePackets, in)
		if err != nil {
			return in, err
		}
		in.Tracks = append(in.Tracks, tr)
		in.Stats = append(in.Stats, stats)
	}
	return in, nil
}

// parseEssenceMap reads the mediasync profile set into per-track placements.
func parseEssenceMap(primer primerMap, k KLV) ([]essenceEntry, error) {
	props, err := newLocalSet(primer, k.Value).all()
	if err != nil {
		return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "essence map: %v", err)
	}
	count, ok := uint32Of(props[tagMapEntries])
	if !ok {
		return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "essence map has no entry count")
	}
	// The map repeats the same tags once per entry; re-walk preserving order.
	ls := newLocalSet(primer, k.Value)
	var entries []essenceEntry
	var cur essenceEntry
	var haveID bool
	for {
		tag, value, ok := ls.next()
		if !ok {
			break
		}
		switch tag {
		case tagMapEntries:
			continue
		case tagMapTrackID:
			if haveID {
				entries = append(entries, cur)
				cur = essenceEntry{}
			}
			id, ok := uint32Of(value)
			if !ok {
				return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "essence map track ID has %d bytes", len(value))
			}
			cur.trackID = id
			haveID = true
		case tagMapKind:
			switch string(value) {
			case "video":
				cur.kind = model.KindVideo
			case "audio":
				cur.kind = model.KindAudio
			case "data":
				cur.kind = model.KindData
			default:
				return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "unknown essence kind %q", string(value))
			}
		case tagMapFirstPacket:
			v, ok := uint32Of(value)
			if !ok {
				return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "first packet has %d bytes", len(value))
			}
			cur.firstPacket = int(v)
		case tagMapPacketCount:
			v, ok := uint32Of(value)
			if !ok {
				return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "packet count has %d bytes", len(value))
			}
			cur.packetCount = int(v)
		case tagMapEditRate:
			r, ok := editRateOf(value)
			if !ok {
				return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "edit rate malformed")
			}
			cur.rate = r
		}
	}
	if haveID {
		entries = append(entries, cur)
	}
	if uint32(len(entries)) != count {
		return nil, model.NewError(model.ErrInvalidKLV, 0, k.Offset,
			"essence map declares %d entries but describes %d", count, len(entries))
	}
	return entries, nil
}

// buildTrack converts one essence placement into a model.Track with sample
// offsets pointing at each KLV value.
func buildTrack(e essenceEntry, essence []KLV, in *Input) (*model.Track, model.TrackStats, error) {
	if e.firstPacket < 0 || e.packetCount < 0 || e.firstPacket+e.packetCount > len(essence) {
		return nil, model.TrackStats{}, model.NewError(model.ErrInvalidKLV, e.trackID, 0,
			"essence range [%d,%d) exceeds %d packets", e.firstPacket, e.firstPacket+e.packetCount, len(essence))
	}
	num, den := e.rate.Rational()
	if num <= 0 || den <= 0 {
		return nil, model.TrackStats{}, model.NewError(model.ErrInvalidKLV, e.trackID, 0, "non-positive edit rate")
	}
	tr := &model.Track{Source: model.SourceMXF, ID: e.trackID, Kind: e.kind, Timescale: uint32(num)}
	// MXF sample duration is one edit unit, expressed in the edit-rate timescale.
	sampleDuration := int64(den)
	var dts int64
	h := sha256.New()
	var total int64
	for i := 0; i < e.packetCount; i++ {
		k := essence[e.firstPacket+i]
		if k.Length > maxSampleValue(e.kind) {
			return nil, model.TrackStats{}, model.NewError(model.ErrSizeLimit, e.trackID, k.Offset,
				"essence packet %d is %d bytes, over the block limit", i, k.Length)
		}
		tr.Samples = append(tr.Samples, model.Sample{
			Index: i, Offset: k.ValueOffset,
			Size: int(k.Length), DTS: dts, PTS: dts, Duration: sampleDuration,
		})
		if _, err := h.Write(k.Value); err != nil {
			return nil, model.TrackStats{}, err
		}
		total += k.Length
		dts += sampleDuration
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	stats := model.TrackStats{TrackID: e.trackID, Kind: e.kind, Samples: len(tr.Samples), Bytes: total, SHA256: digest}
	return tr, stats, nil
}

// maxSampleValue keeps one essence packet under the 64 MiB block budget.
func maxSampleValue(model.TrackKind) int64 { return 64<<20 - 1 }
