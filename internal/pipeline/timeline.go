package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"avrepair/internal/media"
	"avrepair/internal/recovery"
	"avrepair/internal/segment"
)

const maxSegmentsPerTrack = 4096

// capture bundles everything read from one segment file so later stages
// never touch the filesystem again.
type capture struct {
	data      []byte
	format    string // "ts-capture", "ts", "mp4"
	samples   []media.Sample
	missing   int
	recovered int
	unrecov   int
	sha       string
	err       error
}

// loadCapture reads one segment within the size limit and sniffs its
// container format.
func loadCapture(path string, offset, length int64) *capture {
	c := &capture{}
	data, err := media.ReadSegment(path, offset, length)
	if err != nil {
		c.err = err
		return c
	}
	sum := sha256.Sum256(data)
	c.sha = hex.EncodeToString(sum[:])
	c.data = data
	switch {
	case segment.IsPlainTS(data):
		c.format = "ts"
	case len(data) >= segment.RTPHeaderLen && data[0]>>6 == 2:
		c.format = "ts-capture"
	default:
		c.format = "mp4"
	}
	return c
}

// buildTimeline reads every manifest entry, parses timestamps and groups
// entries into tracks in manifest order.
func (c *runContext) buildTimeline() (*media.Timeline, error) {
	man := c.man
	tl := &media.Timeline{Warnings: append([]string(nil), man.Warnings...)}
	byTrack := map[string]*media.Track{}
	var order []string
	for _, e := range man.Entries {
		t, ok := byTrack[e.Track]
		if !ok {
			// Use the timescale the manifest actually declared for this
			// entry; zero defers to the payload (fMP4 mdhd).
			ts := e.Timescale
			if ts == 0 {
				ts = 90000
			}
			t = &media.Track{ID: e.Track, Kind: e.Kind, Source: e.Source, Timescale: ts}
			byTrack[e.Track] = t
			order = append(order, e.Track)
		}
		if len(t.Segments) >= maxSegmentsPerTrack {
			return nil, &media.FaultError{Kind: media.ErrLimitExceeded, Detail: "too many segments on track " + e.Track}
		}
		seg := &media.Segment{
			ID:            fmt.Sprintf("%s/%05d", e.Track, e.Seq),
			Track:         e.Track,
			Kind:          e.Kind,
			Source:        e.Source,
			Seq:           e.Seq,
			URI:           e.URI,
			File:          e.Path,
			DeclaredStart: e.DeclaredStart,
			Duration:      e.Duration,
			Discontinuity: e.Discontinuity,
			Timescale:     t.Timescale,
		}
		cap := loadCapture(e.Path, e.ByteOffset, e.ByteLength)
		c.captures[seg.ID] = cap
		c.parseSegment(seg, cap)
		t.Segments = append(t.Segments, seg)
	}
	for _, id := range order {
		tl.Tracks = append(tl.Tracks, byTrack[id])
	}
	if err := media.CheckTimelineLimit(media.Zero, maxTrackLen(tl)); err != nil {
		return nil, err
	}
	return tl, nil
}

func maxTrackLen(tl *media.Timeline) media.Rational {
	out := media.Zero
	for _, t := range tl.Tracks {
		if l := t.TrackLen(); l.Cmp(out) > 0 {
			out = l
		}
	}
	return out
}

// parseSegment fills samples and packet counters, quarantining on any
// error while leaving the rest of the timeline intact.
func (c *runContext) parseSegment(seg *media.Segment, cap *capture) {
	if cap == nil {
		seg.QuarantineWith(media.ReasonMissingFile, "unreadable")
		return
	}
	if cap.err != nil {
		reason := reasonFor(cap.err)
		if errors.Is(cap.err, media.ErrInvalidTimescale) {
			reason = media.ReasonInvalidTimescale
		}
		seg.QuarantineWith(reason, cap.err.Error())
		return
	}
	seg.SourceSHA = cap.sha
	seg.Format = cap.format
	switch cap.format {
	case "ts-capture":
		c.parseTSCapture(seg, cap)
	case "ts":
		payloads, err := segment.SplitPlainTS(cap.data)
		if err != nil {
			seg.QuarantineWith(media.ReasonTruncated, err.Error())
			return
		}
		samples, err := segment.ParseTS(payloads)
		if err != nil {
			seg.QuarantineWith(media.ReasonUnparseable, err.Error())
			return
		}
		seg.Samples = toModelSamples(samples)
	case "mp4":
		info, err := segment.ParseMP4(cap.data)
		if err != nil {
			seg.QuarantineWith(reasonFor(err), err.Error())
			return
		}
		seg.Timescale = info.Timescale
		samples, err := segment.SamplesFromMP4(cap.data, info)
		if err != nil {
			seg.QuarantineWith(reasonFor(err), err.Error())
			return
		}
		seg.Samples = samples
	}
	if len(seg.Samples) == 0 {
		seg.QuarantineWith(media.ReasonUnparseable, "no timestamps recovered")
		return
	}
	// Record the original clock values before any repair so the journal
	// can report before/after timestamps and source offsets.
	seg.RawStart = seg.Samples[0].Raw
	seg.RawEnd = seg.Samples[len(seg.Samples)-1].Raw
}

func (c *runContext) parseTSCapture(seg *media.Segment, cap *capture) {
	pkts, err := segment.DecodePackets(cap.data)
	if err != nil {
		seg.QuarantineWith(reasonFor(err), err.Error())
		return
	}
	res := recovery.Recover(pkts)
	cap.missing, cap.recovered, cap.unrecov = res.Missing, res.Recovered, len(res.Unrecoverable)
	seg.MissingPackets = res.Missing
	seg.RecoveredPacket = res.Recovered
	seg.UnrecoverGroups = len(res.Unrecoverable)
	if len(res.Packets) == 0 {
		seg.QuarantineWith(media.ReasonUnrecoverable, "capture has no usable packets")
		return
	}
	payloads := make([][]byte, 0, len(res.Packets))
	for _, p := range res.Packets {
		payloads = append(payloads, p.Payload)
	}
	samples, err := segment.ParseTS(payloads)
	if err != nil {
		seg.QuarantineWith(media.ReasonUnparseable, err.Error())
		return
	}
	seg.Samples = toModelSamples(samples)
	if len(res.Unrecoverable) > 0 {
		seg.QuarantineWith(media.ReasonUnrecoverable,
			fmt.Sprintf("%d parity group(s) with multiple losses", len(res.Unrecoverable)))
	}
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, media.ErrUnsafeURI), errors.Is(err, media.ErrPathTraversal):
		return media.ReasonUnsafeURI
	case errors.Is(err, media.ErrLimitExceeded), errors.Is(err, media.ErrInvalidRange):
		return media.ReasonLimitExceeded
	case errors.Is(err, media.ErrInvalidTimescale):
		return media.ReasonInvalidTimescale
	case errors.Is(err, segment.ErrTruncated), errors.Is(err, segment.ErrBadMP4):
		return media.ReasonTruncated
	case errors.Is(err, segment.ErrBadCapture), errors.Is(err, segment.ErrBadTS):
		return media.ReasonUnparseable
	default:
		return media.ReasonUnparseable
	}
}

func toModelSamples(in []segment.Sample) []media.Sample {
	out := make([]media.Sample, 0, len(in))
	for _, s := range in {
		out = append(out, media.Sample{
			PTS:   media.Ticks(s.Raw, s.Scale),
			Sync:  s.Sync,
			Raw:   s.Raw,
			Scale: s.Scale,
		})
	}
	return out
}
