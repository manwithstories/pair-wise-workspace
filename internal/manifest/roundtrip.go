package manifest

import "encoding/json"

// RoundTripScore returns (matched, total) across manifest-level fields,
// representation fields and per-segment fields. A value of 100% means the
// round trip preserved every field and timeline position.
func RoundTripScore(a, b *Manifest) (int, int) {
	total, matched := 0, 0
	check := func(ok bool) {
		total++
		if ok {
			matched++
		}
	}
	check(a.Kind == b.Kind)
	check(a.Version == b.Version)
	check(a.TargetDuration == b.TargetDuration)
	check(a.MediaSequence == b.MediaSequence)
	check(a.PlaylistType == b.PlaylistType)
	check(a.EndList == b.EndList)
	check(a.DASH == b.DASH)
	check(len(a.Representations) == len(b.Representations))
	reps := minInt(len(a.Representations), len(b.Representations))
	for i := 0; i < reps; i++ {
		ar, br := a.Representations[i], b.Representations[i]
		check(ar.ID == br.ID)
		check(ar.Bandwidth == br.Bandwidth)
		check(ar.Codecs == br.Codecs)
		check(ar.MimeType == br.MimeType)
		check(ar.Width == br.Width && ar.Height == br.Height)
		check(ar.InitURI == br.InitURI)
		check(ar.NativeTimescale == br.NativeTimescale)
		check(jsonEqual(ar.Template, br.Template))
		check(len(ar.Segments) == len(br.Segments))
		n := minInt(len(ar.Segments), len(br.Segments))
		for j := 0; j < n; j++ {
			check(jsonEqual(ar.Segments[j], br.Segments[j]))
		}
	}
	return matched, total
}

func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
