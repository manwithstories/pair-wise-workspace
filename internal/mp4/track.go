package mp4

import (
	"io"

	goMp4 "github.com/abema/go-mp4"

	"medpipe/internal/clock"
)

// Kind marks the media role of a track.
type Kind int

const (
	KindUnknown Kind = iota
	KindVideo
	KindAudio
)

// ColorMeta is the source color description that must be preserved (never
// silently overwritten on conflict) when segments are written.
type ColorMeta struct {
	ColorType               string
	ColourPrimaries         uint16
	TransferCharacteristics uint16
	MatrixCoefficients      uint16
	FullRange               bool
}

// SamplePoint is one sample's decode time on the track timescale plus the
// absolute byte offset of its data inside the file.
type SamplePoint struct {
	DTS        clock.Tick
	ByteOffset int64
	Size       uint32
	IsSync     bool
}

// Track is the normalized view consumed by the slicing and audio layers.
type Track struct {
	ID         uint32
	Kind       Kind
	Codec      string
	Timescale  uint32
	Duration   clock.Tick
	Channels   uint16 // audio only
	SampleRate uint32 // audio only, Hz
	Width      uint16 // video only
	Height     uint16
	Color      *ColorMeta
	Samples    []SamplePoint
}

// Container is the full parsed timeline of one MP4/MOV file.
type Container struct {
	Timescale uint32
	Duration  clock.Tick
	Tracks    []*Track
	scan      *Result
}

// Parse scans and probes r. Structural corruption is reported by Scan with
// an exact byte offset; timeline violations carry track, timestamp and
// offset context.
func Parse(r io.ReadSeeker, lim Limits) (*Container, error) {
	scan, err := Scan(r, lim)
	if err != nil {
		return nil, err
	}
	info, err := goMp4.Probe(r)
	if err != nil {
		return nil, clock.E(clock.KindCorrupt, "mp4.parse",
			clock.WithDetail("go-mp4 probe failed"), clock.WithWrap(err))
	}
	// Probe tolerates a truncated final mdat; verify its declared span
	// against the real file size so truncation keeps an exact byte offset.
	for _, m := range scan.Mdat {
		if m.End() > scan.FileSize {
			return nil, clock.E(clock.KindCorrupt, "mp4.parse",
				clock.WithOffset(scan.FileSize),
				clock.WithDetail("mdat at %d truncated: declared end %d, file size %d",
					m.Offset, m.End(), scan.FileSize))
		}
	}
	c := &Container{Timescale: info.Timescale, scan: scan}
	if info.Timescale != 0 {
		c.Duration = clock.MustNew(info.Duration, info.Timescale)
	}
	for _, tr := range info.Tracks {
		t, err := buildTrack(r, tr)
		if err != nil {
			return nil, err
		}
		c.Tracks = append(c.Tracks, t)
	}
	return c, nil
}

func buildTrack(r io.ReadSeeker, tr *goMp4.Track) (*Track, error) {
	t := &Track{ID: tr.TrackID, Timescale: tr.Timescale}
	switch tr.Codec {
	case goMp4.CodecAVC1:
		t.Kind, t.Codec = KindVideo, "avc1"
		if tr.AVC != nil {
			t.Width, t.Height = tr.AVC.Width, tr.AVC.Height
		}
	case goMp4.CodecMP4A:
		t.Kind, t.Codec = KindAudio, "mp4a"
		if tr.MP4A != nil {
			t.Channels = tr.MP4A.ChannelCount
		}
	default:
		t.Kind, t.Codec = KindUnknown, "unknown"
	}
	if tr.Timescale == 0 {
		return nil, clock.E(clock.KindCorrupt, "mp4.track", clock.WithTrack(tr.TrackID),
			clock.WithDetail("zero timescale"))
	}
	t.Duration = clock.MustNew(tr.Duration, tr.Timescale)
	if err := buildTimeline(t, tr); err != nil {
		return nil, err
	}
	if t.Kind == KindAudio {
		if err := fillAudioEntry(r, t); err != nil {
			return nil, err
		}
	}
	if t.Kind == KindVideo {
		if err := fillColor(r, t); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// buildTimeline expands chunk/sample tables into absolute sample points and
// rejects any non-monotonic decode timestamp with track, tick and byte.
func buildTimeline(t *Track, tr *goMp4.Track) error {
	var dts uint64
	si := 0
	for _, ch := range tr.Chunks {
		off := int64(ch.DataOffset)
		for i := uint32(0); i < ch.SamplesPerChunk && si < len(tr.Samples); i++ {
			s := tr.Samples[si]
			if si > 0 && s.TimeDelta == 0 {
				tick, _ := clock.New(dts, tr.Timescale)
				return clock.E(clock.KindCorrupt, "mp4.track", clock.WithTrack(t.ID),
					clock.WithTick(tick), clock.WithOffset(off),
					clock.WithDetail("backward/zero decode timestamp (time_delta=0)"))
			}
			tick, err := clock.New(dts, tr.Timescale)
			if err != nil {
				return clock.E(clock.KindCorrupt, "mp4.track", clock.WithTrack(t.ID),
					clock.WithOffset(off), clock.WithWrap(err))
			}
			if s.CompositionTimeOffset < 0 {
				return clock.E(clock.KindCorrupt, "mp4.track", clock.WithTrack(t.ID),
					clock.WithTick(tick), clock.WithOffset(off),
					clock.WithDetail("backward composition timestamp %d", s.CompositionTimeOffset))
			}
			t.Samples = append(t.Samples, SamplePoint{
				DTS:        tick,
				ByteOffset: off,
				Size:       s.Size,
			})
			off += int64(s.Size)
			dts += uint64(s.TimeDelta)
			si++
		}
	}
	return nil
}
