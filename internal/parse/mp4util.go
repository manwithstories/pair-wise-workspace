package parse

import (
	"bytes"
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// SampleTiming converts a sample DTS to the unified clock.
func (t Track) SampleTiming(s Sample) (clock.Tick, error) {
	if t.Timescale == 0 {
		return 0, fmt.Errorf("parse: track %d has zero timescale", t.ID)
	}
	return clock.ScaleToTicks(int64(s.DTS), t.Timescale)
}

// KeyframeSet returns the sync sample numbers as a set.
func (t Track) KeyframeSet() map[uint32]bool {
	set := make(map[uint32]bool, len(t.Keyframes))
	for _, k := range t.Keyframes {
		set[k] = true
	}
	return set
}

func parseMvhd(children []RawBox) (*mp4.Mvhd, error) {
	raw := FindBox(children, "mvhd")
	if raw == nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: mvhd missing", model.ErrTruncated))
	}
	var box mp4.Mvhd
	if err := unmarshal(raw, &box); err != nil {
		return nil, err
	}
	return &box, nil
}

// mvhdDuration picks the duration field matching the box version.
func mvhdDuration(v0 uint32, v1 uint64, version uint8) uint64 {
	if version == 0 {
		return uint64(v0)
	}
	return v1
}

// unmarshal parses a raw box payload with go-mp4.
func unmarshal(raw *RawBox, dst mp4.IBox) error {
	if raw == nil {
		return fmt.Errorf("%w: nil box", model.ErrInvalid)
	}
	body := raw.BodyBytes()
	r := bytes.NewReader(body)
	if _, err := mp4.Unmarshal(r, uint64(len(body)), dst, mp4.Context{}); err != nil {
		return model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: box %q: %v", model.ErrBadLength, raw.Type, err))
	}
	return nil
}

// FindBox returns the first child with the given type.
func FindBox(children []RawBox, typ string) *RawBox {
	for i := range children {
		if children[i].String() == typ {
			return &children[i]
		}
	}
	return nil
}

// Children returns the children of a container box, or nil.
func (b RawBox) ChildrenOf(typ string) []RawBox {
	for _, c := range b.Children {
		if c.String() == typ {
			return c.Children
		}
	}
	return nil
}

// findPath walks nested containers by name.
func findPath(boxes []RawBox, names ...string) *RawBox {
	var current *RawBox
	list := boxes
	for _, name := range names {
		var found *RawBox
		for i := range list {
			if list[i].String() == name {
				found = &list[i]
				break
			}
		}
		if found == nil {
			return nil
		}
		current = found
		list = found.Children
	}
	return current
}
