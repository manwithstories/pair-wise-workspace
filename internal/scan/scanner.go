// Package scan demuxes media files and builds a structural description of
// every track: time base, keyframe positions, chapters and the timed metadata
// records that must share the audio timeline.
package scan

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/asticode/go-astiav"

	"mediacore/internal/clock"
	"mediacore/internal/meta"
)

// ErrUnsupported reports a container or codec outside the supported set.
var ErrUnsupported = errors.New("scan: unsupported media")

// TrackKind classifies a stream.
type TrackKind string

const (
	TrackVideo    TrackKind = "video"
	TrackAudio    TrackKind = "audio"
	TrackSubtitle TrackKind = "subtitle"
	TrackData     TrackKind = "data"
	TrackUnknown  TrackKind = "unknown"
)

// Track is the scanned description of one stream.
type Track struct {
	Index      int
	Kind       TrackKind
	Codec      string
	CodecID    astiav.CodecID
	TimeBase   clock.Rational
	Start      int64
	Duration   int64
	BitRate    int64
	SampleRate int
	Channels   int
	Width      int
	Height     int
	Extradata  []byte
}

// Keyframe is one random-access point.
type Keyframe struct {
	Track   int
	PTS     int64
	TimeUS  int64
	BytePos int64
	Size    int
}

// MetaRecord is one timed metadata item located on the shared timeline.
type MetaRecord struct {
	Kind        meta.Kind
	Track       int
	TimeUS      int64
	Duration    int64
	PTS         int64
	TimeBase    clock.Rational
	Payload     []byte
	Text        string
	Quarantined bool
	Reason      string
}

// Scan is the complete demux result.
type Scan struct {
	Path       string
	Format     string
	Tracks     []Track
	DurationUS int64
	StartUS    int64
	Keyframes  []Keyframe
	Meta       []MetaRecord
	Chapters   []meta.Chapter
	Packets    int
	Timeline   *clock.Timeline
	// Errors holds non-fatal packet-level failures. Quarantined records are
	// reported through Meta instead of being silently dropped.
	Errors []error
}

// Counts summarises a scan for the terminal report.
func (s *Scan) Counts() (records, quarantined int) {
	for _, m := range s.Meta {
		if m.Quarantined {
			quarantined++
		} else {
			records++
		}
	}
	return records, quarantined
}

// Options bounds scanner work.
type Options struct {
	// MaxRecords bounds the number of metadata records retained.
	MaxRecords int
	// MaxRecordBytes bounds a single metadata payload.
	MaxRecordBytes int
}

func (o Options) withDefaults() Options {
	if o.MaxRecords <= 0 {
		o.MaxRecords = 100_000
	}
	if o.MaxRecordBytes <= 0 {
		o.MaxRecordBytes = meta.MaxRecordBytes
	}
	return o
}

// Open opens a file through the astiav demuxer.
func Open(path string) (*astiav.FormatContext, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("scan: open %q: %w", path, err)
	}
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, errors.New("scan: allocating format context failed")
	}
	if err := fc.OpenInput(path, nil, nil); err != nil {
		fc.Free()
		return nil, fmt.Errorf("scan: open input %q: %w", path, err)
	}
	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("scan: find stream info %q: %w", path, err)
	}
	return fc, nil
}

// Run demuxes path and returns the structural scan.
func Run(path string, opt Options) (result *Scan, err error) {
	opt = opt.withDefaults()
	fc, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		fc.CloseInput()
		fc.Free()
	}()

	s := &Scan{Path: path, Format: fc.InputFormat().Name()}
	if fc.StartTime() != astiav.NoPtsValue && fc.StartTime() > 0 {
		s.StartUS = fc.StartTime()
	}
	if fc.Duration() != astiav.NoPtsValue && fc.Duration() > 0 {
		s.DurationUS = fc.Duration()
	}

	if err := scanTracks(fc, s); err != nil {
		return nil, err
	}
	s.Chapters = scanChapters(fc)
	s.Timeline = buildTimeline(s)
	if err := scanPackets(fc, s, opt); err != nil {
		return nil, err
	}
	return s, nil
}

// buildTimeline creates the shared timeline. A track with an invalid native
// time base has already been rejected in scanTracks, so this cannot lose
// tracks silently; when it still fails we fall back to per-media defaults
// reported through Errors.
func buildTimeline(s *Scan) *clock.Timeline {
	specs := make([]clock.TrackSpec, 0, len(s.Tracks))
	for _, t := range s.Tracks {
		specs = append(specs, clock.TrackSpec{
			Track:    t.Index,
			TimeBase: t.TimeBase,
			Start:    t.Start,
			Duration: t.Duration,
		})
	}
	tl, err := clock.NewTimeline(specs)
	if err != nil {
		s.Errors = append(s.Errors, err)
		return nil
	}
	return tl
}

func scanTracks(fc *astiav.FormatContext, s *Scan) error {
	for _, st := range fc.Streams() {
		cp := st.CodecParameters()
		t := Track{
			Index:      st.Index(),
			CodecID:    cp.CodecID(),
			BitRate:    cp.BitRate(),
			SampleRate: cp.SampleRate(),
			Channels:   cp.ChannelLayout().Channels(),
			Width:      cp.Width(),
			Height:     cp.Height(),
			Start:      st.StartTime(),
			Duration:   st.Duration(),
			Extradata:  append([]byte(nil), cp.ExtraData()...),
		}
		switch cp.MediaType() {
		case astiav.MediaTypeVideo:
			t.Kind = TrackVideo
		case astiav.MediaTypeAudio:
			t.Kind = TrackAudio
		case astiav.MediaTypeSubtitle:
			t.Kind = TrackSubtitle
		case astiav.MediaTypeData:
			t.Kind = TrackData
		default:
			t.Kind = TrackUnknown
		}
		t.Codec = codecName(t.CodecID)
		tb := st.TimeBase()
		if tb.Num() <= 0 || tb.Den() <= 0 {
			return &clock.Error{Code: clock.CodeInvalidTimeBase, Op: "scan.track", Track: t.Index,
				Detail: fmt.Sprintf("timebase %s", tb)}
		}
		r, err := clock.NewRational(int64(tb.Num()), int64(tb.Den()))
		if err != nil {
			return err
		}
		t.TimeBase = r
		if t.Start == astiav.NoPtsValue || t.Start < 0 {
			t.Start = 0
		}
		if t.Duration < 0 {
			t.Duration = clock.NoTimestamp
		}
		s.Tracks = append(s.Tracks, t)
	}
	if len(s.Tracks) == 0 {
		return fmt.Errorf("%w: no tracks", ErrUnsupported)
	}
	return nil
}

func scanChapters(fc *astiav.FormatContext) []meta.Chapter {
	n := fc.NbChapters()
	if n == 0 {
		return nil
	}
	out := make([]meta.Chapter, 0, n)
	for i, ch := range fc.Chapters() {
		chs := meta.Chapter{ID: ch.ID(), Metadata: map[string]string{}}
		if e := ch.Metadata().Get("title", nil, 0); e != nil {
			chs.Title = e.Value()
		}
		tb := clock.Rational{Num: int64(ch.TimeBase().Num()), Den: int64(ch.TimeBase().Den())}
		if !tb.Valid() {
			tb = clock.Microsecond
		}
		start, err := clock.Convert(ch.Start(), tb, clock.Microsecond)
		if err != nil {
			continue
		}
		end, err := clock.Convert(ch.End(), tb, clock.Microsecond)
		if err != nil || end < start {
			continue
		}
		chs.StartUs, chs.EndUs = start, end
		chs.ID = int64(i + 1)
		out = append(out, chs)
	}
	return out
}

// isEOF reports normal end of stream.
func isEOF(err error) bool {
	return errors.Is(err, astiav.ErrEof) || errors.Is(err, io.EOF)
}

func codecName(id astiav.CodecID) string {
	switch id {
	case astiav.CodecIDH264:
		return "h264"
	case astiav.CodecIDH265:
		return "hevc"
	case astiav.CodecIDAac:
		return "aac"
	case astiav.CodecIDMp3:
		return "mp3"
	case astiav.CodecIDPcmS16Le:
		return "pcm_s16le"
	case astiav.CodecIDMovText:
		return "mov_text"
	case astiav.CodecIDSubrip:
		return "subrip"
	case astiav.CodecIDWebvtt:
		return "webvtt"
	case astiav.CodecIDTimedId3:
		return "timed_id3"
	case astiav.CodecIDBinData:
		return "bin_data"
	case astiav.CodecIDScte35:
		return "scte_35"
	}
	return id.String()
}
