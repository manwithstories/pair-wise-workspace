package metadata

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediasync/internal/model"
)

// ColorDesc is the normalized color description carried by a track.
type ColorDesc struct {
	ColorType string // "nclx" for the supported subset
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
}

// CropRect is a pixel-space clean-aperture rectangle.
type CropRect struct {
	X      int64
	Y      int64
	Width  int64
	Height int64
}

// TrackMeta is the normalized metadata record shared by MXF and MP4 sources.
type TrackMeta struct {
	TrackID    string
	Kind       model.TrackKind
	Source     string
	Timescale  int64
	Start      model.Rational // seconds, relative to the shared timeline origin
	FrameRate  model.Rational // frames per second, exact
	Width      int64
	Height     int64
	Rotation   int64 // degrees, clockwise; only cardinal values survive policy
	HasRot     bool
	Crop       *CropRect
	Color      *ColorDesc
}

// ParseResult bundles normalized tracks with every diagnostic raised while
// reading a container. Missing/illegal fields never abort the whole parse.
type ParseResult struct {
	Tracks []TrackMeta
	Diag   model.Bag
}

const mxfHeader = "# mediasync MXF metadata subset v1"

// WriteMXF serializes tracks into the agreed MXF text subset consumed by ParseMXF.
func WriteMXF(w io.Writer, tracks []TrackMeta) error {
	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintln(bw, mxfHeader); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(bw, "container=mxf"); err != nil {
		return err
	}
	for _, t := range tracks {
		lines := []string{
			"track.id=" + t.TrackID,
			"track.kind=" + t.Kind.String(),
			fmt.Sprintf("track.timescale=%d", t.Timescale),
			"track.start=" + t.Start.String(),
			"frame.rate=" + t.FrameRate.String(),
			fmt.Sprintf("frame.width=%d", t.Width),
			fmt.Sprintf("frame.height=%d", t.Height),
		}
		if t.HasRot {
			lines = append(lines, fmt.Sprintf("rotation=%d", t.Rotation))
		}
		if t.Crop != nil {
			c := t.Crop
			lines = append(lines, fmt.Sprintf("crop=%d,%d,%d,%d", c.X, c.Y, c.Width, c.Height))
		}
		if t.Color != nil {
			rng := "limited"
			if t.Color.FullRange {
				rng = "full"
			}
			lines = append(lines, fmt.Sprintf("color=nclx:%d:%d:%d:%s",
				t.Color.Primaries, t.Color.Transfer, t.Color.Matrix, rng))
		}
		for _, ln := range lines {
			if _, err := fmt.Fprintln(bw, ln); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// ParseMXF reads the self-describing MXF text subset. The format is line
// oriented; a new track.id opens a new record.
func ParseMXF(r io.Reader, source string) (*ParseResult, error) {
	res := &ParseResult{}
	sc := bufio.NewScanner(r)
	lineNo := 0
	var cur *TrackMeta
	flush := func() {
		if cur != nil {
			finalizeTrack(cur, &res.Diag)
			res.Tracks = append(res.Tracks, *cur)
		}
	}
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if lineNo == 1 && line != mxfHeader {
			return nil, fmt.Errorf("%s: missing MXF subset header", source)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: malformed line %q", source, lineNo, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "container" {
			if val != "mxf" {
				return nil, fmt.Errorf("%s:%d: unsupported container %q", source, lineNo, val)
			}
			continue
		}
		if key == "track.id" {
			flush()
			cur = &TrackMeta{TrackID: val, Source: source}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("%s:%d: field %q outside a track", source, lineNo, key)
		}
		parseMXFField(cur, key, val, source, lineNo, &res.Diag)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	return res, nil
}

func parseMXFField(t *TrackMeta, key, val, source string, line int, diag *model.Bag) {
	at := model.NoTimestamp
	switch key {
	case "track.kind":
		switch val {
		case "video":
			t.Kind = model.KindVideo
		case "audio":
			t.Kind = model.KindAudio
		default:
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Message: fmt.Sprintf("unknown track kind %q", val)})
		}
	case "track.timescale":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n <= 0 {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Recoverable: false,
				Message: fmt.Sprintf("illegal timescale %q", val)})
			return
		}
		t.Timescale = n
	case "track.start":
		r, err := parseRational(val)
		if err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Message: fmt.Sprintf("illegal start %q: %v", val, err)})
			return
		}
		t.Start = r
	case "frame.rate":
		r, err := parseRational(val)
		if err != nil || r.Num <= 0 {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Message: fmt.Sprintf("illegal frame rate %q", val)})
			return
		}
		t.FrameRate = r
	case "frame.width", "frame.height":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n < 0 {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Message: fmt.Sprintf("illegal %s=%q", key, val)})
			return
		}
		if key == "frame.width" {
			t.Width = n
		} else {
			t.Height = n
		}
	case "rotation":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Message: fmt.Sprintf("illegal rotation %q", val)})
			return
		}
		t.HasRot = true
		t.Rotation = n
	case "crop":
		c, err := parseCrop(val)
		if err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: at, Message: err.Error()})
			return
		}
		t.Crop = c
	case "color":
		c, err := parseColor(val)
		if err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindColorConflict, Severity: model.SeverityWarning,
				Track: t.TrackID, AtTicks: at, Message: err.Error()})
			return
		}
		t.Color = c
	default:
		diag.Add(model.Diagnostic{Kind: model.KindMissingField, Severity: model.SeverityWarning,
			Track: t.TrackID, AtTicks: at, Message: fmt.Sprintf("ignoring unknown field %q", key)})
	}
}

func finalizeTrack(t *TrackMeta, diag *model.Bag) {
	if t.Timescale == 0 {
		diag.Add(model.Diagnostic{Kind: model.KindMissingField, Severity: model.SeverityError,
			Track: t.TrackID, AtTicks: model.NoTimestamp, Recoverable: false,
			Message: "track.timescale missing or invalid"})
	}
	if t.FrameRate.Den == 0 {
		diag.Add(model.Diagnostic{Kind: model.KindMissingField, Severity: model.SeverityError,
			Track: t.TrackID, AtTicks: model.NoTimestamp,
			Message: "frame.rate missing"})
	}
}

func parseRational(s string) (model.Rational, error) {
	numS, denS, ok := strings.Cut(s, "/")
	num, err := strconv.ParseInt(strings.TrimSpace(numS), 10, 64)
	if err != nil {
		return model.Rational{}, err
	}
	den := int64(1)
	if ok {
		den, err = strconv.ParseInt(strings.TrimSpace(denS), 10, 64)
		if err != nil {
			return model.Rational{}, err
		}
	}
	r := model.Rational{Num: num, Den: den}
	return r, r.Validate()
}

func parseCrop(s string) (*CropRect, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("crop needs x,y,width,height got %q", s)
	}
	var vals [4]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("illegal crop component %q", p)
		}
		vals[i] = n
	}
	if vals[2] <= 0 || vals[3] <= 0 {
		return nil, fmt.Errorf("crop width/height must be positive")
	}
	return &CropRect{X: vals[0], Y: vals[1], Width: vals[2], Height: vals[3]}, nil
}

func parseColor(s string) (*ColorDesc, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 5 || parts[0] != "nclx" {
		return nil, fmt.Errorf("color must be nclx:primaries:transfer:matrix:range got %q", s)
	}
	nums := make([]uint16, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseUint(parts[i+1], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("illegal color component %q", parts[i+1])
		}
		nums[i] = uint16(v)
	}
	switch parts[4] {
	case "full", "limited":
	default:
		return nil, fmt.Errorf("color range must be full|limited got %q", parts[4])
	}
	return &ColorDesc{ColorType: "nclx", Primaries: nums[0], Transfer: nums[1],
		Matrix: nums[2], FullRange: parts[4] == "full"}, nil
}
