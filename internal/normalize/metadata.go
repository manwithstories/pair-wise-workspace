// Package normalize resolves conflicting rotation, crop and color
// metadata into a single deterministic decision with an audit trail.
package normalize

import (
	"fmt"

	"archive/internal/ingest"
	"archive/internal/media"
)

// Decision is the resolved metadata applied to every rendered frame.
type Decision struct {
	Rotation int // degrees clockwise: 0/90/180/270
	Crop     *ingest.Crop
	Color    ingest.ColorDesc
	Rules    []string // human-readable audit trail of applied rules
}

// colorPriority ranks color description sources; higher wins.
func colorPriority(source string) int {
	switch source {
	case "mxf-cdci":
		return 2
	default: // mp4 colr
		return 1
	}
}

// Decide merges the MXF CDCI descriptor with MP4 tkhd/clap/colr
// metadata. Rotation follows the tkhd display matrix. Crop rectangles
// outside the coded bounds are rejected. Color conflicts are resolved by
// descriptor priority; the chosen rule is always recorded.
func Decide(mxf *ingest.MXFResult, mp4 *ingest.MP4Result) (*Decision, error) {
	d := &Decision{}
	// --- rotation: display matrix wins over the CDCI rotation flag ---
	rot := 0
	if mxf.HasDesc {
		rot = mxf.Descriptor.Rotation
	}
	if mp4.HasMatrix {
		mr, err := matrixRotation(mp4.Matrix)
		if err != nil {
			return nil, err
		}
		if mxf.HasDesc && rot != mr {
			d.Rules = append(d.Rules, fmt.Sprintf("rotate:tkhd-matrix(%d)>mxf-cdci(%d)", mr, rot))
		} else if mr != 0 {
			d.Rules = append(d.Rules, fmt.Sprintf("rotate:tkhd-matrix(%d)", mr))
		}
		rot = mr
	}
	switch rot {
	case 0, 90, 180, 270:
	default:
		return nil, media.Faultf(media.StageNormalize, -1, 0, "unsupported rotation %d", rot)
	}
	d.Rotation = rot
	// --- crop: must stay inside the coded bounds ---
	if mp4.Crop != nil {
		if !mxf.HasDesc {
			return nil, media.Faultf(media.StageNormalize, -1, 0,
				"clap crop present but no CDCI descriptor to validate against")
		}
		c := mp4.Crop
		if c.X < 0 || c.Y < 0 || c.W <= 0 || c.H <= 0 ||
			c.X+c.W > mxf.Descriptor.Width || c.Y+c.H > mxf.Descriptor.Height {
			return nil, media.Faultf(media.StageNormalize, -1, 0,
				"clap crop %+v out of bounds %dx%d", *c, mxf.Descriptor.Width, mxf.Descriptor.Height)
		}
		d.Crop = c
		d.Rules = append(d.Rules, fmt.Sprintf("crop:clap(%d,%d,%d,%d)", c.X, c.Y, c.W, c.H))
	}
	// --- color: descriptor priority, conflicts recorded ---
	var cands []ingest.ColorDesc
	if mxf.HasDesc {
		cands = append(cands, ingest.ColorDesc{
			Primaries: mxf.Descriptor.Primaries, Transfer: mxf.Descriptor.Transfer,
			Matrix: mxf.Descriptor.Matrix, Source: "mxf-cdci",
		})
	}
	if mp4.Color != nil {
		cands = append(cands, *mp4.Color)
	}
	if len(cands) == 0 {
		d.Color = ingest.ColorDesc{Primaries: 1, Transfer: 1, Matrix: 1, Source: "default-bt709"}
		d.Rules = append(d.Rules, "color:default-bt709")
		return d, nil
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if colorPriority(c.Source) > colorPriority(best.Source) {
			if c.Primaries != best.Primaries || c.Transfer != best.Transfer || c.Matrix != best.Matrix {
				d.Rules = append(d.Rules, fmt.Sprintf("color:%s>lower-priority(%d,%d,%d)",
					c.Source, best.Primaries, best.Transfer, best.Matrix))
			}
			best = c
		} else if c.Primaries != best.Primaries || c.Transfer != best.Transfer || c.Matrix != best.Matrix {
			d.Rules = append(d.Rules, fmt.Sprintf("color:%s>lower-priority(%d,%d,%d)",
				best.Source, c.Primaries, c.Transfer, c.Matrix))
		}
	}
	d.Color = best
	d.Rules = append(d.Rules, fmt.Sprintf("color:use(%s p=%d t=%d m=%d)",
		best.Source, best.Primaries, best.Transfer, best.Matrix))
	return d, nil
}

// matrixRotation maps a tkhd 16.16 fixed-point display matrix to a
// clockwise rotation in degrees. Non-rotation matrices are rejected.
func matrixRotation(m [9]int32) (int, error) {
	const one = 0x00010000
	key := [4]int32{m[0], m[1], m[3], m[4]}
	switch key {
	case [4]int32{one, 0, 0, one}:
		return 0, nil
	case [4]int32{0, one, -one, 0}:
		return 90, nil
	case [4]int32{-one, 0, 0, -one}:
		return 180, nil
	case [4]int32{0, -one, one, 0}:
		return 270, nil
	}
	return 0, media.Faultf(media.StageNormalize, -1, 0,
		"unsupported display matrix %v", m)
}
