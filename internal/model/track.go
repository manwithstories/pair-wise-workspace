package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Container identifies where a track originated.
type Container string

const (
	// MP4 marks tracks parsed from an ISO BMFF container.
	MP4 Container = "mp4"
	// MXF marks tracks parsed from an MXF KLV container.
	MXF Container = "mxf"
	// RTP marks a synthetic transport source in provenance records.
	RTP Container = "rtp"
)

// TrackKind classifies the media type of a track.
type TrackKind string

const (
	// Video is a visual track.
	Video TrackKind = "video"
	// Audio is a sound track.
	Audio TrackKind = "audio"
	// Data is any other track.
	Data TrackKind = "data"
)

// Codec is the sample coding identified by the container.
type Codec string

const (
	// CodecMJPEG is Motion JPEG, the only codec this build decodes.
	CodecMJPEG Codec = "MJPEG"
	// CodecH264 is H.264/AVC.
	CodecH264 Codec = "H264"
	// CodecMPEG2 is MPEG-2 video.
	CodecMPEG2 Codec = "MPEG2"
	// CodecAAC is MPEG-4 audio.
	CodecAAC Codec = "AAC"
	// CodecPCM is linear PCM audio.
	CodecPCM Codec = "PCM"
)

// Decodable reports whether the render stage can produce pixels for the codec.
func (c Codec) Decodable() bool { return c == CodecMJPEG }

// Source identifies which description layer supplied a resolved value.
type Source string

const (
	// SourcePolicy is the caller's explicit policy, highest precedence.
	SourcePolicy Source = "policy"
	// SourceBitstream is the sample description stored with the coded data.
	SourceBitstream Source = "bitstream"
	// SourceContainer is the file-level container description, lowest
	// precedence.
	SourceContainer Source = "container"
)

// Rank orders sources from strongest to weakest.
func (s Source) Rank() int {
	switch s {
	case SourcePolicy:
		return 0
	case SourceBitstream:
		return 1
	case SourceContainer:
		return 2
	}
	return 3
}

// Rect is a crop expressed as insets in pixels.
type Rect struct {
	Left   int
	Top    int
	Right  int
	Bottom int
}

// Empty reports whether the rect crops nothing.
func (r Rect) Empty() bool {
	return r.Left == 0 && r.Top == 0 && r.Right == 0 && r.Bottom == 0
}

// String renders the rect as "l,t,r,b".
func (r Rect) String() string {
	return fmt.Sprintf("%d,%d,%d,%d", r.Left, r.Top, r.Right, r.Bottom)
}

// ParseRect parses "l,t,r,b".
func ParseRect(s string) (Rect, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return Rect{}, fmt.Errorf("model: crop %q must be l,t,r,b", s)
	}
	var v [4]int
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return Rect{}, fmt.Errorf("model: crop component %q invalid", p)
		}
		v[i] = n
	}
	return Rect{Left: v[0], Top: v[1], Right: v[2], Bottom: v[3]}, nil
}

// Description is the resolved visual description of a track.
type Description struct {
	Width, Height int
	Rotation      int // 0, 90, 180 or 270 degrees clockwise
	Crop          Rect
	Primaries     uint16
	Transfer      uint16
	Matrix        uint16
	FullRange     bool
	ColorKnown    bool
}

// Claim is one description value together with the layer that supplied it.
type Claim struct {
	Value  string
	Source Source
}

// Claims holds the raw description values per precedence layer. Keys are
// "rotation", "crop", "primaries", "transfer", "matrix" and "range".
type Claims struct {
	Policy    map[string]string
	Bitstream map[string]string
	Container map[string]string
}

// Get returns the claim for key in the given layer.
func (c Claims) Get(src Source, key string) (string, bool) {
	var m map[string]string
	switch src {
	case SourcePolicy:
		m = c.Policy
	case SourceBitstream:
		m = c.Bitstream
	case SourceContainer:
		m = c.Container
	}
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	return v, ok
}

// Sample is a single timed sample (edit unit) of a track. DTS and Duration are
// expressed in the track's own Timebase; conversion to the public timebase is
// mandatory before any comparison.
type Sample struct {
	Index    int
	TrackID  uint32
	Offset   int64 // byte offset of the payload within the container
	Size     int64
	DTS      int64
	Duration int64
	Sync     bool
	Codec    Codec
}

// End returns the exclusive end byte of the sample payload.
func (s Sample) End() int64 { return s.Offset + s.Size }

// Track is the container-neutral view of one track.
type Track struct {
	Number     int // deterministic order number assigned after parsing
	Container  Container
	ID         uint32
	Kind       TrackKind
	Codec      Codec
	Timebase   Timebase
	Duration   int64 // in Timebase units
	Samples    []Sample
	Claims     Claims
	Resolved   Description
	DescSource map[string]Source

	EssenceStart int64
	EssenceEnd   int64

	// Isolated tracks are excluded from composition and rendering because a
	// description could not be trusted.
	Isolated bool
}

// Label returns a stable human readable identity such as "mp4#1".
func (t *Track) Label() string {
	return fmt.Sprintf("%s#%d", t.Container, t.ID)
}

// DTS90k returns the sample start time converted to the 90 kHz public timebase.
func (t *Track) DTS90k(s Sample) (int64, error) {
	if t.Timebase == (Timebase{}) {
		return 0, fmt.Errorf("track %s has no timebase", t.Label())
	}
	return t.Timebase.ConvertTo(s.DTS, RTP90k)
}

// Payload returns the sample bytes from the container buffer.
func (t *Track) Payload(data []byte, s Sample) ([]byte, error) {
	if s.Offset < 0 || s.Size < 0 || s.End() > int64(len(data)) {
		return nil, fmt.Errorf("track %s sample %d range [%d,%d) outside buffer of %d bytes",
			t.Label(), s.Index, s.Offset, s.End(), len(data))
	}
	return data[s.Offset:s.End()], nil
}

// SampleByIndex returns the sample with the given index.
func (t *Track) SampleByIndex(i int) (Sample, bool) {
	if i < 0 || i >= len(t.Samples) {
		return Sample{}, false
	}
	return t.Samples[i], true
}

// FieldTable renders all comparable description fields of the track as
// "scope/name=value" strings. It is used for round-trip consistency checks and
// digests; map iteration is avoided so the order is always stable.
func (t *Track) FieldTable() []string {
	out := []string{
		fmt.Sprintf("%s/container=%s", t.Label(), t.Container),
		fmt.Sprintf("%s/id=%d", t.Label(), t.ID),
		fmt.Sprintf("%s/kind=%s", t.Label(), t.Kind),
		fmt.Sprintf("%s/codec=%s", t.Label(), t.Codec),
		fmt.Sprintf("%s/timebase=%s", t.Label(), t.Timebase),
		fmt.Sprintf("%s/duration=%d", t.Label(), t.Duration),
		fmt.Sprintf("%s/samples=%d", t.Label(), len(t.Samples)),
	}
	keys := make([]string, 0, len(t.Claims.Policy)+len(t.Claims.Bitstream)+len(t.Claims.Container))
	for _, m := range []map[string]string{t.Claims.Policy, t.Claims.Bitstream, t.Claims.Container} {
		for k := range m {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		for _, src := range []Source{SourcePolicy, SourceBitstream, SourceContainer} {
			if v, ok := t.Claims.Get(src, k); ok {
				out = append(out, fmt.Sprintf("%s/claim.%s.%s=%s", t.Label(), src, k, v))
			}
		}
	}
	for _, s := range t.Samples {
		out = append(out, fmt.Sprintf("%s/sample.%d=%d,%d,%d,%t", t.Label(), s.Index, s.DTS, s.Duration, s.Size, s.Sync))
	}
	return out
}
