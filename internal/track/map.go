// Package track deterministically maps demuxed tracks to output
// slots by language, codec and role; conflicts are reported, never
// silently overwritten.
package track

import (
	"sort"

	"archive/internal/demux"
	"archive/internal/fault"
)

// Version identifies the mapping rules recorded in checkpoints.
const Version = "v1"

// Role derives the output role from the track handler type.
func Role(t demux.Track) string {
	switch t.Handler {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	default:
		return "aux"
	}
}

func roleOrder(r string) int {
	switch r {
	case "video":
		return 0
	case "audio":
		return 1
	default:
		return 2
	}
}

// Map assigns each input track an output index ordered by
// (role, language, codec, track ID). Two tracks sharing the same
// (role, language, codec) key conflict and produce an error.
func Map(tracks []demux.Track) ([]int, error) {
	if len(tracks) == 0 {
		return nil, fault.Newf(fault.Fatal, "trackmap", "", "no tracks to map")
	}
	type keyed struct {
		idx               int
		role, lang, codec string
		id                uint32
	}
	seen := map[string]uint32{}
	ks := make([]keyed, len(tracks))
	for i, t := range tracks {
		r := Role(t)
		k := r + "|" + t.Lang + "|" + t.Codec
		if prev, dup := seen[k]; dup {
			return nil, fault.Newf(fault.Fatal, "trackmap", "",
				"conflict: tracks %d and %d share %q", prev, t.ID, k)
		}
		seen[k] = t.ID
		ks[i] = keyed{i, r, t.Lang, t.Codec, t.ID}
	}
	sort.SliceStable(ks, func(a, b int) bool {
		x, y := ks[a], ks[b]
		if roleOrder(x.role) != roleOrder(y.role) {
			return roleOrder(x.role) < roleOrder(y.role)
		}
		if x.lang != y.lang {
			return x.lang < y.lang
		}
		if x.codec != y.codec {
			return x.codec < y.codec
		}
		return x.id < y.id
	})
	out := make([]int, len(tracks))
	for pos, k := range ks {
		out[k.idx] = pos
	}
	return out, nil
}
