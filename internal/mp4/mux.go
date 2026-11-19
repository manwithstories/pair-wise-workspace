// Package mp4 muxes the deliverable into an ISO-BMFF file with go-mp4 and can
// re-read it to confirm the sample tables survived the round trip. It is the
// container boundary of the pipeline: everything above works in media time,
// everything below is bytes.
package mp4

import (
	"io"

	"github.com/abema/go-mp4"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// TrackSamples are the samples that belong to one track in the deliverable.
type TrackSamples struct {
	Track     string
	Kind      timeline.NodeKind
	Timescale uint32
	// Keyframes are the sync samples. Between two keyframes the muxer emits
	// evenly spaced non-sync samples, so the sample table reflects the full
	// declared span without storing every frame.
	Keyframes    []Sample
	TotalSamples int
	// Span is the declared presentation interval.
	Span timeline.Span
}

// Sample is one indexed sample.
type Sample struct {
	PTS  timeline.Micros
	Size int
	Key  bool
}

// muxTimescale is the movie timescale written into mvhd.
const muxTimescale = 90000

// MaxTracks bounds container work.
const MaxTracks = 64

// Write muxes tracks into a deterministic ISO-BMFF byte stream.
func Write(w io.Writer, tracks []TrackSamples) error {
	data, err := Build(tracks)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// Build returns the muxed container bytes.
//
// The file is laid out as ftyp | moov | mdat. Because chunk offsets live inside
// moov, the muxer writes moov twice: once to learn its size, then again with
// the now-known mdat base offset. moov's size does not depend on the offset
// values (they are fixed-width), so the second pass is exact.
func Build(tracks []TrackSamples) ([]byte, error) {
	if len(tracks) == 0 {
		return nil, &core.Error{Code: core.CodeEmptyTrack, Detail: "no tracks to mux", Fatal: true}
	}
	if len(tracks) > MaxTracks {
		return nil, &core.Error{Code: core.CodeOverLimit, Detail: "too many tracks to mux", Fatal: true}
	}
	for _, t := range tracks {
		if t.Timescale == 0 {
			return nil, &core.Error{Code: core.CodeBadTimestamp, Track: t.Track,
				Detail: "track timescale is zero: container time base cannot be determined", Fatal: true}
		}
	}

	payload, chunks := buildMedia(tracks)
	ftyp := writeFtyp()

	// Pass 1: moov with zero offsets, only to measure it.
	probe, err := writeMoov(tracks, chunks, 0)
	if err != nil {
		return nil, err
	}
	mdatHeader := 8 // size + 'mdat'
	if len(payload)+mdatHeader >= 1<<32 {
		return nil, &core.Error{Code: core.CodeOverLimit, Detail: "mdat exceeds 32-bit offsets", Fatal: true}
	}
	mdatBase := uint32(len(ftyp) + len(probe) + mdatHeader)

	// Pass 2: moov with the real base offset.
	moov, err := writeMoov(tracks, chunks, mdatBase)
	if err != nil {
		return nil, err
	}
	if len(moov) != len(probe) {
		return nil, &core.Error{Code: core.CodeInternal, Detail: "moov size changed between passes", Fatal: true}
	}

	out := make([]byte, 0, len(ftyp)+len(moov)+mdatHeader+len(payload))
	out = append(out, ftyp...)
	out = append(out, moov...)
	mdat := makeMdatHeader(len(payload))
	out = append(out, mdat...)
	out = append(out, payload...)
	return out, nil
}

// ContainerInfo is the round-trip summary read back from the muxed bytes.
type ContainerInfo struct {
	Tracks     int
	Timescale  uint32
	Duration   uint64
	TrackTypes []string
	Chunks     int
	Samples    int
}

// Inspect re-reads a container and reports its structure. It is used by the
// pipeline to confirm the deliverable is navigable, not merely written.
func Inspect(data []byte) (*ContainerInfo, error) {
	if len(data) == 0 {
		return nil, &core.Error{Code: core.CodeBadIndex, Detail: "empty container", Fatal: true}
	}
	r := newByteReader(data)
	info := &ContainerInfo{}
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeEdts():
			if h.BoxInfo.Type == mp4.BoxTypeTrak() {
				info.Tracks++
				info.TrackTypes = append(info.TrackTypes, "")
			}
			// Container boxes carry no fields we need; descend into children.
			return h.Expand()
		case mp4.BoxTypeMvhd():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			if mvhd, ok := box.(*mp4.Mvhd); ok {
				info.Timescale = mvhd.Timescale
				info.Duration = mvhd.GetDuration()
			}
		case mp4.BoxTypeHdlr():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			if hdlr, ok := box.(*mp4.Hdlr); ok {
				ctype := string(hdlr.HandlerType[:])
				if n := len(info.TrackTypes); n > 0 {
					info.TrackTypes[n-1] = ctype
				}
			}
		case mp4.BoxTypeStco():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			if stco, ok := box.(*mp4.Stco); ok {
				info.Chunks += len(stco.ChunkOffset)
			}
		case mp4.BoxTypeStsz():
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			if stsz, ok := box.(*mp4.Stsz); ok {
				info.Samples += int(stsz.SampleCount)
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, &core.Error{Code: core.CodeBadIndex, Detail: "container re-read failed: " + err.Error(), Fatal: true}
	}
	if info.Tracks == 0 {
		return nil, &core.Error{Code: core.CodeBadIndex, Detail: "container has no tracks", Fatal: true}
	}
	if info.Timescale == 0 {
		return nil, &core.Error{Code: core.CodeBadTimestamp, Detail: "container timescale missing", Fatal: true}
	}
	return info, nil
}
