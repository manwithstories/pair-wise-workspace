// Package segment produces a deterministic SHA-256 digest for each output
// timeline segment. The canonical encoding is stable across runs and
// platforms: it sorts all collections, uses fixed-width decimal and
// length-prefixed strings, so identical segment facts always hash identically.
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// Recovery classifies how a segment was produced.
type Recovery string

const (
	// Clean means every contributing event was well-formed.
	Clean Recovery = "clean"
	// Recovered means the segment contains a locatable, repaired fault.
	Recovered Recovery = "recovered"
	// Failed means the segment could not be produced.
	Failed Recovery = "failed"
)

// Source pins a segment back to an input location.
type Source struct {
	Kind        string
	Ref         string
	ByteOffset  int64
	PacketIndex int
	CueIndex    int
}

// Record is the canonical set of facts hashed for one segment.
type Record struct {
	Index        int
	StartNs      int64
	EndNs        int64
	Recovery     Recovery
	TrackIDs     []uint32
	Sources      []Source
	IssueClasses []string
	// LateEvents counts events that arrived after the watermark and forced a
	// replay. It is part of the digest so a replayed segment is auditable.
	LateEvents int
}

// Canonical renders r as a stable byte string. Every variable-length
// collection is sorted and every field is length-prefixed or fixed-width, so
// two equal records cannot encode differently.
func Canonical(r Record) []byte {
	b := make([]byte, 0, 256)
	field := func(s string) {
		b = strconv.AppendInt(b, int64(len(s)), 10)
		b = append(b, ':')
		b = append(b, s...)
		b = append(b, '|')
	}
	num := func(n int64) {
		b = strconv.AppendInt(b, n, 10)
		b = append(b, '|')
	}
	field("seg")
	num(int64(r.Index))
	num(r.StartNs)
	num(r.EndNs)
	field(string(r.Recovery))
	num(int64(r.LateEvents))

	tracks := append([]uint32(nil), r.TrackIDs...)
	sort.Slice(tracks, func(i, j int) bool { return tracks[i] < tracks[j] })
	field("tracks")
	for _, t := range tracks {
		num(int64(t))
	}

	classes := append([]string(nil), r.IssueClasses...)
	sort.Strings(classes)
	field("classes")
	for _, c := range classes {
		field(c)
	}

	srcs := append([]Source(nil), r.Sources...)
	sort.Slice(srcs, func(i, j int) bool {
		if srcs[i].Kind != srcs[j].Kind {
			return srcs[i].Kind < srcs[j].Kind
		}
		if srcs[i].Ref != srcs[j].Ref {
			return srcs[i].Ref < srcs[j].Ref
		}
		if srcs[i].ByteOffset != srcs[j].ByteOffset {
			return srcs[i].ByteOffset < srcs[j].ByteOffset
		}
		if srcs[i].PacketIndex != srcs[j].PacketIndex {
			return srcs[i].PacketIndex < srcs[j].PacketIndex
		}
		return srcs[i].CueIndex < srcs[j].CueIndex
	})
	field("sources")
	for _, s := range srcs {
		field(s.Kind)
		field(s.Ref)
		num(s.ByteOffset)
		num(int64(s.PacketIndex))
		num(int64(s.CueIndex))
	}
	return b
}

// SHA256 returns the lowercase hex digest of the canonical record.
func SHA256(r Record) string {
	sum := sha256.Sum256(Canonical(r))
	return hex.EncodeToString(sum[:])
}

// Short returns the first n hex characters of the digest (12 in the CLI).
func Short(hexDigest string, n int) string {
	if n <= 0 || n >= len(hexDigest) {
		return hexDigest
	}
	return hexDigest[:n]
}
