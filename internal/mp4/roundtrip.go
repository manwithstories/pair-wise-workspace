package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediapack/internal/fault"
)

// RoundTrip is the result of parsing and re-serializing one file.
type RoundTrip struct {
	InputDigest  string
	OutputDigest string
	InputBytes   int
	OutputBytes  int
	ByteEqual    bool
	FieldsMatch  bool
	// MatchPct is 100.00 when every compared field and every byte matches.
	MatchPct float64
	// Differences lists the first mismatching fields, empty on success.
	Differences []string
	Summary     *Summary
	Unknown     int
}

// Check parses data, serializes the tree again, decodes the output, and
// compares both the key fields and the byte stream.  A truncated box, an
// illegal length or a malformed payload returns an attributable fault; an
// unknown box is preserved verbatim and does not by itself fail the check.
func Check(data []byte, limits Limits) (*RoundTrip, error) {
	original, err := Parse(data, limits)
	if err != nil {
		return nil, err
	}
	want, err := original.Summary(limits)
	if err != nil {
		return nil, err
	}

	out, err := Serialize(original)
	if err != nil {
		return nil, err
	}
	again, err := Parse(out, limits)
	if err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "mp4", "*", "reserialize-failed",
			"re-serialized file no longer parses", err)
	}
	got, err := again.Summary(limits)
	if err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "mp4", "*", "resummary-failed",
			"re-serialized file has no summary", err)
	}

	rt := &RoundTrip{
		InputDigest:  digest(data),
		OutputDigest: digest(out),
		InputBytes:   len(data),
		OutputBytes:  len(out),
		ByteEqual:    bytes.Equal(data, out),
		Summary:      want,
		Unknown:      len(original.Unknown),
	}
	rt.Differences = compareSummaries(want, got)

	total := 6 + len(want.Tracks)*11 // top-level fields plus per-track fields
	bad := len(rt.Differences)
	rt.FieldsMatch = bad == 0
	if total > 0 {
		rt.MatchPct = float64(total-bad) / float64(total) * 100
		if rt.MatchPct < 0 {
			rt.MatchPct = 0
		}
	}
	if !rt.ByteEqual && rt.FieldsMatch {
		rt.Differences = append(rt.Differences, "byte-stream")
		rt.MatchPct = 0
	}
	return rt, nil
}

// Serialize writes the tree back out.  Unknown boxes and opaque payloads are
// copied byte for byte so that a file containing vendor boxes round-trips.
func Serialize(f *File) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeNodes(&buf, f.Nodes, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeNodes(buf *bytes.Buffer, nodes []*Node, depth int) error {
	if depth > 64 {
		return fault.New(fault.ClassLimit, "mp4", "*", "too-deep", "serialization nesting too deep")
	}
	for _, n := range nodes {
		if err := writeNode(buf, n, depth); err != nil {
			return err
		}
	}
	return nil
}

func writeNode(buf *bytes.Buffer, n *Node, depth int) error {
	if n.Payload == nil {
		return writeRawBox(buf, n.Type, n.Raw, n.ExtendEOF)
	}
	payload, err := marshalPayload(n.Payload)
	if err != nil {
		return fault.Wrap(fault.ClassInternal, "mp4", "*", "marshal-failed",
			fmt.Sprintf("cannot serialize %s", n.Type), err)
	}
	var body bytes.Buffer
	body.Write(payload)
	if len(n.Children) > 0 {
		if err := writeNodes(&body, n.Children, depth+1); err != nil {
			return err
		}
	}
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header, uint32(body.Len()+8))
	copy(header[4:], n.Type[:])
	buf.Write(header)
	buf.Write(body.Bytes())
	return nil
}

// writeRawBox reproduces an opaque box exactly, including an original
// "extends to end of file" size field.
func writeRawBox(buf *bytes.Buffer, typ mp4.BoxType, raw []byte, extendEOF bool) error {
	size := len(raw) + 8
	header := make([]byte, 8)
	if extendEOF {
		binary.BigEndian.PutUint32(header, 0)
	} else {
		binary.BigEndian.PutUint32(header, uint32(size))
	}
	copy(header[4:], typ[:])
	buf.Write(header)
	buf.Write(raw)
	return nil
}

func marshalPayload(box mp4.IBox) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, box, mp4.Context{}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func compareSummaries(want, got *Summary) []string {
	var diffs []string
	top := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			diffs = append(diffs, fmt.Sprintf("%s: %v != %v", name, a, b))
		}
	}
	top("major_brand", want.MajorBrand, got.MajorBrand)
	top("minor_version", want.MinorVersion, got.MinorVersion)
	top("compatible_brands", want.Compatible, got.Compatible)
	top("timescale", want.Timescale, got.Timescale)
	top("duration", want.Duration, got.Duration)
	top("track_count", len(want.Tracks), len(got.Tracks))

	for i := 0; i < len(want.Tracks) && i < len(got.Tracks); i++ {
		a, b := want.Tracks[i], got.Tracks[i]
		prefix := fmt.Sprintf("track[%d]", i)
		top(prefix+".id", a.ID, b.ID)
		top(prefix+".handler", a.Handler, b.Handler)
		top(prefix+".timescale", a.Timescale, b.Timescale)
		top(prefix+".duration", a.Duration, b.Duration)
		top(prefix+".sample_count", a.SampleCount, b.SampleCount)
		top(prefix+".sample_deltas", a.SampleDeltas, b.SampleDeltas)
		top(prefix+".keyframes", a.Keyframes, b.Keyframes)
		top(prefix+".width", a.Width, b.Width)
		top(prefix+".height", a.Height, b.Height)
		top(prefix+".channels", a.Channels, b.Channels)
		top(prefix+".sample_rate", a.SampleRate, b.SampleRate)
		top(prefix+".color", colorString(a.Color), colorString(b.Color))
	}
	return diffs
}

func colorString(c *Color) string {
	if c == nil {
		return "none"
	}
	return fmt.Sprintf("%d/%d/%d/%t", c.Primaries, c.Transfer, c.Matrix, c.FullRange)
}
