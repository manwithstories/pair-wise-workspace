package ingest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediameld/internal/model"
)

// klvTriplet is one decoded 16-byte-key / BER-length / value unit.
type klvTriplet struct {
	Key        [16]byte
	ValueStart int64
	Length     int
	Offset     int64
}

// The fixture's private keys mirror the KLV set used by SMPTE 405M: a header,
// a track list, a body containing per-track metadata and sample runs, and a
// trailing terminator.
var (
	klvHeaderKey    = klvKey(0x01)
	klvTrackListKey = klvKey(0x02)
	klvBodyKey      = klvKey(0x03)
	klvTailKey      = klvKey(0x04)
	klvTrackKey     = klvKey(0x10)
	klvSamplesKey   = klvKey(0x11)
)

func klvKey(b byte) [16]byte {
	var k [16]byte
	k[15] = b
	return k
}

// MXFResult is the outcome of demuxing an MXF/KLV stream. Tracks that are
// unaffected by a truncated tail are still returned, with Err describing the
// isolated failure.
type MXFResult struct {
	Tracks     []*model.Track
	Err        error // non-nil if a KLV unit was isolated
	Recovered  int   // number of intact tracks
	TailOffset int64
}

// DemuxMXF parses a KLV stream. A malformed OR over-running KLV length is
// reported with ERR_KLV_LENGTH and its byte offset; parsing stops at that
// point but every fully decoded track before it is returned, so a corrupt tail
// cannot discard healthy audio/video.
func DemuxMXF(r io.ReadSeeker, sourcePath string) (MXFResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return MXFResult{}, err
	}
	units, tailErr := parseKLV(data)
	if tailErr != nil && len(units) == 0 {
		// Nothing at all could be decoded: this is a fatal input error.
		return MXFResult{}, tailErr
	}
	res := MXFResult{Err: tailErr}
	if tailErr != nil {
		if te, ok := tailErr.(*model.Error); ok && te.HasOffset {
			res.TailOffset = te.Offset
		}
	}
	// Locate the body unit; per-track units live inside it. If the body is
	// truncated we still parse the tracks that arrived whole.
	body, ok := findUnit(units, klvBodyKey)
	if !ok {
		// Fall back to top-level track units (older MXF layout).
		body = klvTriplet{ValueStart: 0, Length: 0}
	}
	var trackUnits, sampleUnits []klvTriplet
	if body.Length > 0 {
		inner, innerErr := parseKLVRegion(data, body.ValueStart, body.ValueStart+int64(body.Length))
		if innerErr != nil {
			res.Err = innerErr
			if te, ok := innerErr.(*model.Error); ok && te.HasOffset {
				res.TailOffset = te.Offset
			}
		}
		trackUnits = unitsWithKey(inner, klvTrackKey)
		sampleUnits = unitsWithKey(inner, klvSamplesKey)
	} else {
		trackUnits = unitsWithKey(units, klvTrackKey)
		sampleUnits = unitsWithKey(units, klvSamplesKey)
	}

	for i, tu := range trackUnits {
		meta := string(data[tu.ValueStart : tu.ValueStart+int64(tu.Length)])
		spec, err := parseTrackMeta(meta)
		if err != nil {
			return res, err
		}
		if i >= len(sampleUnits) {
			// A missing sample run is itself an isolated tail failure: keep
			// the tracks already decoded and record a gap.
			res.Err = model.NewError(model.CodeKLVStructure, "track %d has no sample run", spec.ID).WithTrack(fmt.Sprint(spec.ID))
			break
		}
		su := sampleUnits[i]
		samples, err := parseSamplesValue(data[su.ValueStart : su.ValueStart+int64(su.Length)])
		if err != nil {
			res.Err = model.TagOffset(model.Tag(err, fmt.Sprint(spec.ID)), su.Offset)
			break
		}
		track := &model.Track{
			ID:         fmt.Sprint(spec.ID),
			Kind:       spec.Kind,
			Timescale:  spec.Timescale,
			SampleDesc: spec.Desc,
			SourcePath: sourcePath,
			Display: model.DisplayMeta{
				Rotation: spec.Rotation, Width: spec.Width, Height: spec.Height,
			},
			CodedWidth:    spec.Width,
			CodedHeight:   spec.Height,
			TrackRotation: spec.Rotation,
			TrackColor:    spec.Color,
			TrackCrop:     spec.Crop,
		}
		for _, s := range samples {
			sum := sha256.Sum256(s.Data)
			track.Samples = append(track.Samples, model.Sample{
				Index: len(track.Samples), DTS: s.DTS, PTS: s.PTS, Duration: s.Dur,
				Size: int32(len(s.Data)), Hash: sum,
				SourceOffset: su.ValueStart + s.RelOffset,
			})
		}
		res.Tracks = append(res.Tracks, track)
	}
	if res.Err != nil {
		// Every fully decoded track counts as recovered from the damage.
		res.Recovered = len(res.Tracks)
	}
	return res, nil
}

type sampleValue struct {
	Data          []byte
	RelOffset     int64 // byte offset of Data within the samples value
	DTS, PTS, Dur int64
}

func parseSamplesValue(v []byte) ([]sampleValue, error) {
	if len(v) < 4 {
		return nil, model.NewError(model.CodeKLVStructure, "samples value too short")
	}
	count := int(beU32(v[0:]))
	pos := 4
	out := make([]sampleValue, 0, count)
	for i := 0; i < count; i++ {
		if pos+28 > len(v) {
			return out, model.NewError(model.CodeKLVStructure, "sample %d header truncated", i)
		}
		dts := int64(beU64(v[pos:]))
		pts := int64(beU64(v[pos+8:]))
		dur := int64(beU64(v[pos+16:]))
		size := int(beU32(v[pos+24:]))
		pos += 28
		if pos+size > len(v) {
			return out, model.NewError(model.CodeKLVStructure, "sample %d payload truncated", i)
		}
		out = append(out, sampleValue{Data: v[pos : pos+size], RelOffset: int64(pos), DTS: dts, PTS: pts, Dur: dur})
		pos += size
	}
	return out, nil
}

type trackMeta struct {
	ID        uint32
	Kind      model.TrackKind
	Timescale int64
	Desc      string
	Width     int
	Height    int
	Rotation  int
	Color     model.ColorDesc
	Crop      model.Crop
}

func parseTrackMeta(s string) (trackMeta, error) {
	m := trackMeta{Kind: model.KindVideo}
	if len(s) < 5 {
		return m, model.NewError(model.CodeKLVStructure, "empty track metadata")
	}
	// The value opens with a big-endian 32-bit track id, then "key=value;"
	// pairs.
	m.ID = beU32([]byte(s[0:4]))
	rest := s[4:]
	m.Color = model.UnknownColor("mxf-none")
	for _, f := range strings.Split(rest, ";") {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "kind":
			if v == "audio" {
				m.Kind = model.KindAudio
			}
		case "timescale":
			m.Timescale, _ = strconv.ParseInt(v, 10, 64)
		case "desc":
			m.Desc = v
		case "w":
			m.Width, _ = strconv.Atoi(v)
		case "h":
			m.Height, _ = strconv.Atoi(v)
		case "rot":
			m.Rotation, _ = strconv.Atoi(v)
		case "color":
			m.Color = parseColorTag(v)
		case "crop":
			m.Crop = parseCropTag(v)
		}
	}
	if m.Timescale <= 0 {
		return m, model.NewError(model.CodeKLVStructure, "track %d has non-positive timescale", m.ID)
	}
	return m, nil
}

func parseColorTag(v string) model.ColorDesc {
	parts := strings.Split(v, ":")
	if len(parts) != 5 || parts[0] != "nclx" {
		return model.UnknownColor(parts[0])
	}
	p, _ := strconv.Atoi(parts[1])
	tr, _ := strconv.Atoi(parts[2])
	mx, _ := strconv.Atoi(parts[3])
	return model.ColorDesc{
		Primaries: primariesName(uint16(p)), Transfer: transferName(uint16(tr)),
		Matrix: matrixName(uint16(mx)), FullRange: rangeName(parts[4] == "true"),
		OriginalTag: v,
	}
}

func parseCropTag(v string) model.Crop {
	nums := strings.Split(v, ",")
	if len(nums) != 4 {
		return model.Crop{}
	}
	x, _ := strconv.Atoi(nums[0])
	y, _ := strconv.Atoi(nums[1])
	w, _ := strconv.Atoi(nums[2])
	h, _ := strconv.Atoi(nums[3])
	return model.Crop{Valid: true, X: x, Y: y, W: w, H: h}
}

// parseKLV decodes a top-level KLV stream. On a bad length it returns every
// unit decoded so far plus an ERR_KLV_LENGTH error carrying the offset.
func parseKLV(data []byte) ([]klvTriplet, error) {
	return parseKLVRegion(data, 0, int64(len(data)))
}

func parseKLVRegion(data []byte, start, end int64) ([]klvTriplet, error) {
	var out []klvTriplet
	pos := start
	for pos < end {
		if end-pos < 17 {
			return out, model.NewError(model.CodeKLVLength,
				"%d trailing bytes cannot form a KLV triplet", end-pos).WithOffset(pos)
		}
		var key [16]byte
		copy(key[:], data[pos:pos+16])
		lenPos := pos + 16
		valLen, berSize, err := parseBER(data, lenPos, end)
		if err != nil {
			return out, model.TagOffset(err, pos)
		}
		valueStart := lenPos + int64(berSize)
		if valueStart+int64(valLen) > end {
			return out, model.NewError(model.CodeKLVLength,
				"KLV length %d overruns region: value starts %d, region ends %d", valLen, valueStart, end).WithOffset(pos)
		}
		out = append(out, klvTriplet{Key: key, ValueStart: valueStart, Length: int(valLen), Offset: pos})
		pos = valueStart + int64(valLen)
	}
	return out, nil
}

// parseBER decodes an SMPTE long-form BER length (1 to 8 subsequent bytes).
func parseBER(data []byte, pos, end int64) (int64, int, error) {
	if pos >= end {
		return 0, 0, model.NewError(model.CodeKLVLength, "missing BER length")
	}
	first := data[pos]
	if first&0x80 == 0 {
		return int64(first), 1, nil
	}
	n := int(first & 0x7f)
	if n == 0 {
		return 0, 0, model.NewError(model.CodeKLVLength, "indefinite BER length is not allowed in MXF")
	}
	if n > 8 {
		return 0, 0, model.NewError(model.CodeKLVLength, "BER length uses %d bytes (max 8)", n)
	}
	if pos+1+int64(n) > end {
		return 0, 0, model.NewError(model.CodeKLVLength, "BER length truncated")
	}
	var v int64
	for i := 0; i < n; i++ {
		v = v<<8 | int64(data[pos+1+int64(i)])
	}
	return v, 1 + n, nil
}

func findUnit(units []klvTriplet, key [16]byte) (klvTriplet, bool) {
	for _, u := range units {
		if u.Key == key {
			return u, true
		}
	}
	return klvTriplet{}, false
}

func unitsWithKey(units []klvTriplet, key [16]byte) []klvTriplet {
	var out []klvTriplet
	for _, u := range units {
		if u.Key == key {
			out = append(out, u)
		}
	}
	return out
}

func beU64(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}
