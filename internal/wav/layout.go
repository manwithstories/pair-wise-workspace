package wav

import "avcheck/internal/fault"

// Layout identifies a channel layout.
type Layout int

const (
	LayoutUnknown Layout = iota
	Mono
	Stereo
	Quad
	Surround51
)

// Channels returns the channel count of the layout.
func (l Layout) Channels() int {
	switch l {
	case Mono:
		return 1
	case Stereo:
		return 2
	case Quad:
		return 4
	case Surround51:
		return 6
	}
	return 0
}

func (l Layout) String() string {
	switch l {
	case Mono:
		return "mono"
	case Stereo:
		return "stereo"
	case Quad:
		return "quad"
	case Surround51:
		return "5.1"
	}
	return "unknown"
}

// ParseLayout maps a tag to a layout; unknown tags are locatable errors.
func ParseLayout(tag string) (Layout, error) {
	switch tag {
	case "mono":
		return Mono, nil
	case "stereo":
		return Stereo, nil
	case "quad":
		return Quad, nil
	case "5.1":
		return Surround51, nil
	}
	return LayoutUnknown, fault.New(fault.Unsupported, "wav.layout", -1,
		"unknown layout tag "+tag)
}

// LayoutFromTag maps the wire tag byte to a layout.
func LayoutFromTag(b byte) (Layout, error) {
	tags := map[byte]string{1: "mono", 2: "stereo", 3: "quad", 4: "5.1"}
	if s, ok := tags[b]; ok {
		return ParseLayout(s)
	}
	return LayoutUnknown, fault.New(fault.Unsupported, "wav.layout", int64(b),
		"unknown layout tag byte")
}

// Tag returns the wire tag byte for the layout.
func (l Layout) Tag() byte {
	switch l {
	case Mono:
		return 1
	case Stereo:
		return 2
	case Quad:
		return 3
	case Surround51:
		return 4
	}
	return 0
}

// Matrix returns per-channel downmix gains to stereo [left, right].
func (l Layout) Matrix() [][2]float64 {
	switch l {
	case Mono:
		return [][2]float64{{0.7071, 0.7071}}
	case Stereo:
		return [][2]float64{{1, 0}, {0, 1}}
	case Quad: // FL FR RL RR
		return [][2]float64{{1, 0}, {0, 1}, {0.7071, 0}, {0, 0.7071}}
	case Surround51: // FL FR C LFE SL SR
		return [][2]float64{{1, 0}, {0, 1}, {0.7071, 0.7071}, {0.5, 0.5}, {0.7071, 0}, {0, 0.7071}}
	}
	return nil
}
