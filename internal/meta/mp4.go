package meta

import (
	"bytes"
	"fmt"
	"math"

	gomp4 "github.com/abema/go-mp4"

	"relaycheck/internal/media"
)

// MP4Track is the raw track-level claims extracted from an MP4.
type MP4Track struct {
	ID          int64
	Type        string
	TimeScale   uint32
	Duration    int64
	Width       uint32
	Height      uint32
	Matrix      [9]int32
	Rotation    Rotation
	TrackColor  *Color
	Crop        *Crop
}

// MP4Meta is the extracted MP4 metadata before reconciliation.
type MP4Meta struct {
	Brand          string
	Tracks         []*MP4Track
	ContainerColor *Color
}

func pathHas(p gomp4.BoxPath, want string) bool {
	for _, bt := range p {
		if bt.String() == want {
			return true
		}
	}
	return false
}

// rotationFromMatrix converts a tkhd transformation matrix to whole degrees.
// The fixed-point scale cancels in atan2. Quarter-turn matrices classify
// exactly; an arbitrary angle (e.g. 45) is returned as-is for validation.
func rotationFromMatrix(m [9]int32) Rotation {
	angle := math.Atan2(float64(m[1]), float64(m[0])) * 180 / math.Pi
	d := int(math.Round(angle))
	d = ((d % 360) + 360) % 360
	return Rotation{Degrees: d}
}

var primariesNames = map[uint16]string{1: "bt709", 9: "bt2020", 5: "bt601", 6: "bt601"}
var transferNames = map[uint16]string{1: "bt709", 16: "smpte2084", 18: "arib-std-b67", 4: "gamma22"}
var matrixNames = map[uint16]string{1: "bt709", 9: "bt2020nc", 6: "bt601"}

func mapNCLX(c *gomp4.Colr) *Color {
	out := &Color{
		ColourType: string(c.ColourType[:]),
		FullRange:  c.FullRangeFlag,
	}
	p, pok := primariesNames[c.ColourPrimaries]
	t, tok := transferNames[c.TransferCharacteristics]
	m, mok := matrixNames[c.MatrixCoefficients]
	if c.ColourType == [4]byte{'n', 'c', 'l', 'x'} && pok && tok && mok {
		out.Primaries, out.Transfer, out.Matrix = p, t, m
		out.Known = true
		return out
	}
	// Preserve the unknown marker rather than guessing a standard.
	out.RawMarker = c.ColourPrimaries
	out.Primaries = fmt.Sprintf("unknown(%d)", c.ColourPrimaries)
	out.Transfer = fmt.Sprintf("unknown(%d)", c.TransferCharacteristics)
	out.Matrix = fmt.Sprintf("unknown(%d)", c.MatrixCoefficients)
	out.Known = false
	return out
}

func decodeClap(p []byte) *Crop {
	if len(p) < 32 {
		return nil
	}
	u32 := func(i int) uint32 {
		return uint32(p[i])<<24 | uint32(p[i+1])<<16 | uint32(p[i+2])<<8 | uint32(p[i+3])
	}
	wN, wD := u32(0), u32(4)
	hN, hD := u32(8), u32(12)
	// hOff at 16/20, vOff at 24/28 (not needed for bound classification here).
	if wD == 0 || hD == 0 {
		return nil
	}
	return &Crop{W: int64(wN / wD), H: int64(hN / hD)}
}

// ReadMP4 parses MP4 metadata via go-mp4, then enforces strict length/sample
// safety. On corruption the returned Fault carries the best-known track id
// and exact time range.
func ReadMP4(data []byte) (meta *MP4Meta, retErr *media.Fault) {
	defer func() {
		if r := recover(); r != nil {
			retErr = media.NewFault(media.CodeParseError, "mp4 parser rejected malformed box: %v", r)
			enrichFromMeta(retErr, meta)
		}
	}()

	m := &MP4Meta{}
	var cur *MP4Track
	r := bytes.NewReader(data)

	_, err := gomp4.ReadBoxStructure(r, func(h *gomp4.ReadHandle) (any, error) {
		typ := h.BoxInfo.Type.String()
		switch typ {
		case "trak":
			tr := &MP4Track{}
			m.Tracks = append(m.Tracks, tr)
			prev := cur
			cur = tr
			vals, exErr := h.Expand()
			cur = prev
			return vals, exErr
		case "tkhd":
			box, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, perr
			}
			if tk, ok := box.(*gomp4.Tkhd); ok && cur != nil {
				cur.ID = int64(tk.TrackID)
				if tk.Version == 1 {
					cur.Duration = int64(tk.DurationV1)
				} else {
					cur.Duration = int64(tk.DurationV0)
				}
				cur.Width, cur.Height = tk.Width>>16, tk.Height>>16
				cur.Matrix = tk.Matrix
				cur.Rotation = rotationFromMatrix(tk.Matrix)
			}
			return nil, nil
		case "mdhd":
			box, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, perr
			}
			if md, ok := box.(*gomp4.Mdhd); ok && cur != nil {
				cur.TimeScale = md.Timescale
				if md.Version == 1 {
					cur.Duration = int64(md.DurationV1)
				} else {
					cur.Duration = int64(md.DurationV0)
				}
			}
			return nil, nil
		case "colr":
			box, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, perr
			}
			if c, ok := box.(*gomp4.Colr); ok {
				col := mapNCLX(c)
				if pathHas(h.Path, "stsd") && cur != nil {
					cur.TrackColor = col
				} else {
					m.ContainerColor = col
				}
			}
			return nil, nil
		case "clap":
			if cur != nil {
				var buf bytes.Buffer
				if _, derr := h.ReadData(&buf); derr == nil {
					if c := decodeClap(buf.Bytes()); c != nil {
						c.FrameW, c.FrameH = int64(cur.Width), int64(cur.Height)
						c.X = 0
						c.Y = 0
						cur.Crop = c
					}
				}
			}
			return nil, nil
		}
		if containerBoxes[typ] {
			return h.Expand()
		}
		return nil, nil
	})

	// Strict safety scan gives stable fault codes for corrupt lengths/samples.
	if sf := scanBoxesSafe(data); sf != nil {
		enrichFromMeta(sf, m)
		return m, sf
	}
	if err != nil {
		f := media.NewFault(media.CodeParseError, "go-mp4: %v", err)
		enrichFromMeta(f, m)
		return m, f
	}
	return m, nil
}

func enrichFromMeta(f *media.Fault, m *MP4Meta) {
	if f == nil || m == nil || len(m.Tracks) == 0 {
		return
	}
	t := m.Tracks[0]
	if t.ID != 0 {
		f.WithTrack(t.ID)
	}
	if t.TimeScale != 0 {
		if end, ef := media.Rat(t.Duration, int64(t.TimeScale)); ef == nil {
			f.WithRange(media.MustRat(0, 1), end)
		}
	}
}

// Reconcile applies precedence to each MP4 track and returns the descriptor.
func (m *MP4Meta) Reconcile(container Layer, override Layer) (*Descriptor, *media.Fault) {
	d := &Descriptor{Container: "mp4"}
	def := DefaultLayer()
	if m.ContainerColor != nil && container.Color == nil {
		container.Color = m.ContainerColor
	}
	for _, t := range m.Tracks {
		trackLayer := Layer{
			Rotation: &t.Rotation,
			Crop:     t.Crop,
			Color:    t.TrackColor,
		}
		rt, f := ReconcileTrack(ReconcileInput{
			TrackID: t.ID, Type: t.Type, TimeScale: t.TimeScale, Duration: t.Duration,
			Width: t.Width, Height: t.Height,
			Override: override, Track: trackLayer, Container: container, Default: def,
		})
		if f != nil {
			return d, f
		}
		d.Tracks = append(d.Tracks, rt)
		d.Conflicts = append(d.Conflicts, rt.Conflicts...)
	}
	return d, nil
}
