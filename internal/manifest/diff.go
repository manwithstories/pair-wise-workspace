package manifest

import "fmt"

// Diff describes the first field that differs between two manifests.
type Diff struct {
	Field string
	Want  string
	Got  string
}

func (d Diff) String() string {
	return fmt.Sprintf("field %s: want %s, got %s", d.Field, d.Want, d.Got)
}

// Compare returns the first differing field between two manifests, or nil
// when they are semantically equal. The comparison is format-agnostic, so a
// DASH MPD and an HLS bundle may be compared after projection onto Manifest.
func Compare(want, got *Manifest) *Diff {
	if want.Kind != got.Kind {
		return &Diff{"kind", string(want.Kind), string(got.Kind)}
	}
	if want.TargetTick != got.TargetTick {
		return &Diff{"target_tick", fmt.Sprint(want.TargetTick), fmt.Sprint(got.TargetTick)}
	}
	if want.MinBufferTick != got.MinBufferTick {
		return &Diff{"min_buffer_tick", fmt.Sprint(want.MinBufferTick),
			fmt.Sprint(got.MinBufferTick)}
	}
	if len(want.Representations) != len(got.Representations) {
		return &Diff{"representation_count",
			fmt.Sprint(len(want.Representations)), fmt.Sprint(len(got.Representations))}
	}
	for ri := range want.Representations {
		if d := compareRep(ri, &want.Representations[ri], &got.Representations[ri]); d != nil {
			return d
		}
	}
	return nil
}

func compareRep(ri int, want, got *Representation) *Diff {
	prefix := fmt.Sprintf("representations[%d].", ri)
	if want.ID != got.ID {
		return &Diff{prefix + "id", want.ID, got.ID}
	}
	if want.Bandwidth != got.Bandwidth {
		return &Diff{prefix + "bandwidth",
			fmt.Sprint(want.Bandwidth), fmt.Sprint(got.Bandwidth)}
	}
	if want.Width != got.Width {
		return &Diff{prefix + "width", fmt.Sprint(want.Width), fmt.Sprint(got.Width)}
	}
	if want.Height != got.Height {
		return &Diff{prefix + "height", fmt.Sprint(want.Height), fmt.Sprint(got.Height)}
	}
	if want.MIME != got.MIME {
		return &Diff{prefix + "mime", want.MIME, got.MIME}
	}
	if len(want.Segments) != len(got.Segments) {
		return &Diff{prefix + "segment_count",
			fmt.Sprint(len(want.Segments)), fmt.Sprint(len(got.Segments))}
	}
	for si := range want.Segments {
		if d := compareSegment(ri, si, &want.Segments[si], &got.Segments[si]); d != nil {
			return d
		}
	}
	return nil
}

func compareSegment(ri, si int, want, got *Segment) *Diff {
	prefix := fmt.Sprintf("representations[%d].segments[%d].", ri, si)
	checks := []struct {
		name string
		want any
		got  any
	}{
		{"index", want.Index, got.Index},
		{"url", want.URL, got.URL},
		{"start_tick", want.StartTick, got.StartTick},
		{"duration_tick", want.DurationTick, got.DurationTick},
		{"epoch", want.Epoch, got.Epoch},
		{"discontinuity", want.Discontinuity, got.Discontinuity},
		{"keyframe", want.Keyframe, got.Keyframe},
	}
	for _, c := range checks {
		if fmt.Sprint(c.want) != fmt.Sprint(c.got) {
			return &Diff{prefix + c.name, fmt.Sprint(c.want), fmt.Sprint(c.got)}
		}
	}
	return nil
}
