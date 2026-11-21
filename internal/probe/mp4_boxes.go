package probe

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// rawClap is a parsed ISO/IEC 14496-12 clean-aperture box. go-mp4 v1.4.1
// does not model clap, so the payload is decoded here.
type rawClap struct {
	cleanWidth  clock.Rational
	cleanHeight clock.Rational
	horizOffset clock.Rational
	vertOffset  clock.Rational
}

func parseClap(payload []byte) (rawClap, error) {
	if len(payload) != 32 {
		return rawClap{}, fmt.Errorf("clap payload is %d bytes, want 32", len(payload))
	}
	vals := make([]int64, 8)
	for i := range vals {
		vals[i] = int64(int32(binary.BigEndian.Uint32(payload[i*4:])))
	}
	f := func(n, d int64, what string) (clock.Rational, error) {
		if d == 0 {
			return clock.Rational{}, fmt.Errorf("clap %s denominator is zero", what)
		}
		return clock.New(n, d)
	}
	var err error
	var c rawClap
	if c.cleanWidth, err = f(vals[0], vals[1], "width"); err != nil {
		return rawClap{}, err
	}
	if c.cleanHeight, err = f(vals[2], vals[3], "height"); err != nil {
		return rawClap{}, err
	}
	if c.horizOffset, err = f(vals[4], vals[5], "horizontal offset"); err != nil {
		return rawClap{}, err
	}
	if c.vertOffset, err = f(vals[6], vals[7], "vertical offset"); err != nil {
		return rawClap{}, err
	}
	return c, nil
}

// crop converts the clean aperture into edge insets for a coded frame.
// The clean aperture is centred on the coded frame plus an offset, so
// left = (coded-clean)/2 + horizOffset.
func (c rawClap) crop(codedW, codedH uint32) (model.Crop, error) {
	toU32 := func(r clock.Rational, name string) (uint32, error) {
		v := r.Round()
		if v < 0 || v > math.MaxUint32 {
			return 0, fmt.Errorf("clap %s %v out of range", name, r)
		}
		return uint32(v), nil
	}
	cw, err := toU32(c.cleanWidth, "width")
	if err != nil {
		return model.Crop{}, err
	}
	ch, err := toU32(c.cleanHeight, "height")
	if err != nil {
		return model.Crop{}, err
	}
	if cw > codedW || ch > codedH {
		return model.Crop{}, fmt.Errorf("clap %dx%d exceeds coded %dx%d", cw, ch, codedW, codedH)
	}
	left, err := clock.New(int64(codedW-cw), 2)
	if err != nil {
		return model.Crop{}, err
	}
	if left, err = left.Add(c.horizOffset); err != nil {
		return model.Crop{}, err
	}
	top, err := clock.New(int64(codedH-ch), 2)
	if err != nil {
		return model.Crop{}, err
	}
	if top, err = top.Add(c.vertOffset); err != nil {
		return model.Crop{}, err
	}
	var crop model.Crop
	if crop.Left, err = toU32(left, "left inset"); err != nil {
		return model.Crop{}, err
	}
	if crop.Top, err = toU32(top, "top inset"); err != nil {
		return model.Crop{}, err
	}
	if crop.Left+cw > codedW || crop.Top+ch > codedH {
		return model.Crop{}, fmt.Errorf("clap leaves frame bounds: left=%d top=%d clean=%dx%d coded=%dx%d",
			crop.Left, crop.Top, cw, ch, codedW, codedH)
	}
	crop.Right = codedW - crop.Left - cw
	crop.Bottom = codedH - crop.Top - ch
	return crop, nil
}

// rotationFromMatrix converts a tkhd 3x3 transform into a right-angle
// rotation in degrees, choosing the nearest of 0/90/180/270.
func rotationFromMatrix(m [9]int32) (int, error) {
	const scale = 65536.0
	a, b := float64(m[0])/scale, float64(m[1])/scale
	c, d := float64(m[3])/scale, float64(m[4])/scale
	det := a*d - b*c
	if math.Abs(det) < 1e-6 {
		return 0, fmt.Errorf("tkhd matrix is singular (det=%g)", det)
	}
	if det < 0 {
		return 0, fmt.Errorf("tkhd matrix is mirrored (det=%g); not a right-angle rotation", det)
	}
	deg := math.Atan2(b, a) * 180 / math.Pi
	q := int(math.Round(deg/90)) * 90 % 360
	if q < 0 {
		q += 360
	}
	// Verify the matrix really is that pure rotation: both columns must be
	// unit length and orthogonal after snapping.
	cq, sq := math.Cos(float64(q)*math.Pi/180), math.Sin(float64(q)*math.Pi/180)
	if math.Abs(a-cq) > 1e-3 || math.Abs(b-sq) > 1e-3 || math.Abs(c+sq) > 1e-3 || math.Abs(d-cq) > 1e-3 {
		return 0, fmt.Errorf("tkhd matrix is not an axis-aligned rotation (rotates %g degrees)", deg)
	}
	return q, nil
}

// readLimited reads exactly size bytes, refusing payloads over limit.
func readLimited(r io.Reader, size uint64, limit int) ([]byte, error) {
	if size > uint64(limit) {
		return nil, model.New(model.KindOverLimit, "probe", fmt.Sprintf("box payload %d exceeds limit %d", size, limit))
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
