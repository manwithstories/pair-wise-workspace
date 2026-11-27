package mp4

import (
	"io"
	"os"
	"sort"
	"time"

	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
)

// Epoch1904 is the ISO-BMFF reference date (1904-01-01 UTC).
var Epoch1904 = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// Sample describes one indexed sample with exact rational timing.
type Sample struct {
	Index      int       // 0-based decode order
	Offset     int64     // absolute file offset
	Size       int64
	DTS        clock.Rat // decode time relative to track media origin
	PTS        clock.Rat // presentation time (DTS + composition offset)
	Duration   clock.Rat
	Sync       bool // keyframe / IDR
}

// EditEntry is one elst entry; times are converted to seconds.
type EditEntry struct {
	SegmentDuration clock.Rat // in movie time
	MediaTime       clock.Rat // in track media time; negative means empty edit
	Empty           bool
}

// TrackIndex is the parsed view of one trak box.
type TrackIndex struct {
	TrackID   uint32
	Video     bool
	Codec     string // hvc1/avc1/mp4a/...
	Timescale uint32 // mdhd timescale; 0 is rejected while parsing
	Duration  clock.Rat
	Samples   []Sample
	Edits     []EditEntry
	// LargestReorder is the maximum decode->presentation rank distance; it
	// exposes B-frame reorder depth for PTS validation.
	LargestReorder int
}

// MovieIndex is everything the pipeline needs from the moov box.
type MovieIndex struct {
	Path          string
	MovieTimescale uint32
	MovieDuration  clock.Rat
	CreationTime   time.Time // mvhd creation_time, zero when unset
	Tracks         []*TrackIndex
	Video          *TrackIndex
	Audio          *TrackIndex
	Header         *Header
}

// KeyframeTimes returns presentation times of sync samples in increasing order.
func (t *TrackIndex) KeyframeTimes() []clock.Rat {
	out := make([]clock.Rat, 0, len(t.Samples)/16+1)
	for i := range t.Samples {
		if t.Samples[i].Sync {
			out = append(out, t.Samples[i].PTS)
		}
	}
	return out
}

// IndexFile scans and indexes one container without loading mdat contents.
func IndexFile(f *os.File) (*MovieIndex, error) {
	hdr, err := ScanHeader(f)
	if err != nil {
		return nil, err
	}
	idx := &MovieIndex{Path: f.Name(), Header: hdr}
	if err := parseMoov(f, hdr.Moov, idx); err != nil {
		if me, ok := media.As(err); ok && me.Path == "" {
			me.Path = f.Name()
		}
		return nil, err
	}
	if idx.Video == nil {
		return nil, media.New(media.PhaseIndex, media.KindUnsupported, f.Name(),
			hdr.Moov.Offset, "no supported video track found")
	}
	return idx, nil
}

// parseMoov reads mvhd and walks every trak subtree.
func parseMoov(ra io.ReaderAt, moov Box, idx *MovieIndex) error {
	mvhd, ok, err := FindBox(ra, moov.Payload, moov.ContentEnd(), "mvhd")
	if err != nil {
		return err
	}
	if !ok {
		return media.New(media.PhaseIndex, media.KindTruncated, "", moov.Offset, "mvhd box missing")
	}
	if err := parseMVHD(ra, mvhd, idx); err != nil {
		return err
	}
	return Walk(ra, moov.Payload, moov.ContentEnd(), func(b Box) error {
		if b.Type != "trak" {
			return nil
		}
		tr, err := parseTrack(ra, b, idx.Header)
		if err != nil {
			return err
		}
		idx.Tracks = append(idx.Tracks, tr)
		switch {
		case tr.Video && idx.Video == nil:
			idx.Video = tr
		case !tr.Video && idx.Audio == nil:
			idx.Audio = tr
		}
		return nil
	})
}

func parseMVHD(ra io.ReaderAt, b Box, idx *MovieIndex) error {
	buf, err := ReadPayload(ra, b, 1<<20)
	if err != nil {
		return err
	}
	version, _, rest := FullBoxVersion(buf)
	at := 0
	var creation, mod uint64
	var ts uint32
	var dur64 uint64
	if version == 1 {
		if creation, at, err = readU64(rest, at, b.Payload); err != nil {
			return err
		}
		if mod, at, err = readU64(rest, at, b.Payload); err != nil {
			return err
		}
		if v, a2, e := readU32(rest, at, b.Payload); e != nil {
			return e
		} else {
			ts, at = v, a2
		}
		if dur64, at, err = readU64(rest, at, b.Payload); err != nil {
			return err
		}
	} else {
		var c32, m32, d32 uint32
		if c32, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		if m32, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		if ts, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		if d32, at, err = readU32(rest, at, b.Payload); err != nil {
			return err
		}
		creation, mod, dur64 = uint64(c32), uint64(m32), uint64(d32)
	}
	_ = mod
	if ts == 0 {
		return media.New(media.PhaseIndex, media.KindBadTimebase, "", b.Payload,
			"mvhd declares zero movie timescale")
	}
	idx.MovieTimescale = ts
	d, err := clock.New(int64(dur64), int64(ts))
	if err != nil {
		return media.New(media.PhaseIndex, media.KindBadTimebase, "", b.Payload,
			"invalid movie duration/timebase").Wrap(err)
	}
	idx.MovieDuration = d
	if creation != 0 {
		idx.CreationTime = Epoch1904.Add(time.Duration(creation) * time.Second)
	}
	return nil
}

// presentationOrder returns sample indices sorted by PTS, then DTS.
func presentationOrder(samples []Sample) []int {
	order := make([]int, len(samples))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		x, y := samples[order[a]], samples[order[b]]
		if c := x.PTS.Cmp(y.PTS); c != 0 {
			return c < 0
		}
		return x.DTS.Cmp(y.DTS) < 0
	})
	return order
}
