// Package segment demuxes MPEG-TS and CMAF/fMP4 media segments into packets
// with absolute timestamps, payload bytes and source byte offsets, and reports
// container faults against those offsets.
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"aircheck/internal/media"
)

// Format identifies the container of a segment.
type Format int

const (
	FormatUnknown Format = iota
	FormatTS
	FormatCMAF
)

// String renders the format for reports.
func (f Format) String() string {
	switch f {
	case FormatTS:
		return "ts"
	case FormatCMAF:
		return "cmaf"
	}
	return "unknown"
}

// Track describes the elementary stream carried by a segment.
type Track struct {
	Kind       media.Kind
	Codec      string
	Timescale  int64 // ticks per second for DTS/PTS in packets
	SampleRate int
	Channels   int
	Width      int
	Height     int
	TrackID    uint32
	// Init is the raw initialisation section (moov or in-band parameter
	// metadata) as read from the source; remux re-emits it unchanged.
	Init []byte
}

// Packet is one elementary payload unit: a video access unit or an audio
// access unit. DTS/PTS are track ticks; Duration is in the same ticks.
type Packet struct {
	Kind         media.Kind
	TrackID      uint32
	DTS          int64
	PTS          int64
	Duration     int64
	Samples      int // audio sample count, 0 for video
	Keyframe     bool
	Data         []byte
	SourceOffset int64 // absolute offset in the source file of the unit
}

// End returns the DTS-based end instant of the packet.
func (p Packet) End() int64 { return p.DTS + p.Duration }

// Result is one demuxed segment. Packets are ordered by DTS then source offset.
type Result struct {
	Format    Format
	Artifact  string
	Tracks    []Track
	Packets   []Packet
	Wraps     int // 33-bit MPEG-TS timestamp wraps normalised
	Offset0   int64
	SizeBytes int64
	// InitBytes is the byte length of the initialisation section, 0 for TS.
	InitBytes int64
}

// TrackFor returns the track of the requested kind.
func (r *Result) TrackFor(k media.Kind) (Track, bool) {
	for _, t := range r.Tracks {
		if t.Kind == k {
			return t, true
		}
	}
	return Track{}, false
}

// TrackByID returns the track with the given identifier.
func (r *Result) TrackByID(id uint32) (Track, bool) {
	for _, t := range r.Tracks {
		if t.TrackID == id {
			return t, true
		}
	}
	return Track{}, false
}

func trackByID(m map[uint16]Track, id uint32) (Track, bool) {
	t, ok := m[uint16(id)]
	return t, ok
}

// PayloadBytes totals elementary payload bytes.
func (r *Result) PayloadBytes() int64 {
	var n int64
	for _, p := range r.Packets {
		n += int64(len(p.Data))
	}
	return n
}

// Counts returns the video frame count, keyframe count and audio sample count.
func (r *Result) Counts() (frames, keyframes, samples int) {
	for _, p := range r.Packets {
		switch p.Kind {
		case media.KindVideo:
			frames++
			if p.Keyframe {
				keyframes++
			}
		case media.KindAudio:
			samples += p.Samples
		}
	}
	return
}

// FirstDTS returns the earliest DTS among packets, in the track clock of the
// given kind. It fails if the segment has no packets of that kind.
func (r *Result) FirstDTS(k media.Kind) (int64, error) {
	found := false
	var v int64
	for _, p := range r.Packets {
		if p.Kind != k {
			continue
		}
		if !found || p.DTS < v {
			v, found = p.DTS, true
		}
	}
	if !found {
		return 0, fmt.Errorf("segment %s has no %s packet", r.Artifact, k)
	}
	return v, nil
}

// LastEnd returns the latest DTS+Duration among packets of a kind.
func (r *Result) LastEnd(k media.Kind) (int64, error) {
	found := false
	var v int64
	for _, p := range r.Packets {
		if p.Kind != k {
			continue
		}
		if !found || p.End() > v {
			v, found = p.End(), true
		}
	}
	if !found {
		return 0, fmt.Errorf("segment %s has no %s packet", r.Artifact, k)
	}
	return v, nil
}

// Sort orders packets by track then DTS then source offset.
func (r *Result) Sort() {
	sort.SliceStable(r.Packets, func(i, j int) bool {
		a, b := r.Packets[i], r.Packets[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.DTS != b.DTS {
			return a.DTS < b.DTS
		}
		return a.SourceOffset < b.SourceOffset
	})
}

// PayloadHash is the sha256 over the concatenation of per-packet sha256
// digests. It is invariant to container framing: a TS payload remuxed into a
// fresh TS segment, or an fMP4 sample re-fragmented, yields the same value.
// Hashing includes kind and sample count so a mis-tracked packet is visible.
func (r *Result) PayloadHash() [sha256.Size]byte {
	h := sha256.New()
	for _, p := range r.Packets {
		d := sha256.Sum256(p.Data)
		fmt.Fprintf(h, "%d:%d:%d:%x\n", p.Kind, len(p.Data), p.Samples, d[:8])
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// PayloadHashString renders the digest as lowercase hex.
func (r *Result) PayloadHashString() string {
	h := r.PayloadHash()
	return hex.EncodeToString(h[:])
}

// FillMissingDurations gives packets without an explicit duration the interval
// to the next packet of the same track; the last packet in a track inherits
// the previous interval. Remuxers need a duration for every unit.
func (r *Result) FillMissingDurations(defaults map[media.Kind]int64) {
	for k, def := range defaults {
		var idx []int
		for i := range r.Packets {
			if r.Packets[i].Kind == k {
				idx = append(idx, i)
			}
		}
		for j, i := range idx {
			if r.Packets[i].Duration > 0 {
				continue
			}
			if j+1 < len(idx) {
				next := r.Packets[idx[j+1]].DTS
				if next > r.Packets[i].DTS {
					r.Packets[i].Duration = next - r.Packets[i].DTS
					continue
				}
			}
			if j > 0 && r.Packets[idx[j-1]].Duration > 0 {
				r.Packets[i].Duration = r.Packets[idx[j-1]].Duration
				continue
			}
			r.Packets[i].Duration = def
		}
	}
}
