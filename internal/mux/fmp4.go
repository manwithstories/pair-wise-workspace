package mux

import (
	"encoding/binary"
	"fmt"

	"github.com/abema/go-mp4"

	"mediacheck/internal/media"
)

// Sample is one framed media sample on the 90 kHz timeline.
type Sample struct {
	PTS      int64
	Duration int64
	Data     []byte
	Keyframe bool
}

// Fragment is one track run, normally one keyframe-aligned video segment.
type Fragment struct {
	TrackID uint32
	BasePTS int64
	Samples []Sample
}

// Config controls the deterministic fMP4 init boxes.
type Config struct {
	DurationTicks uint32
	Width         uint16
	Height        uint16
	AudioChannels uint16
}

// SegmentInfo records output bytes for per-segment bitrate verification.
type SegmentInfo struct {
	BasePTS       int64
	DurationTicks int64
	Bytes         int
}

// Result is the serialized fMP4 and segment byte accounting.
type Result struct {
	Bytes    []byte
	Segments []SegmentInfo
}

// BuildFMP4 writes ftyp + moov and keyframe-aligned video fragments followed by
// one audio fragment. It uses abema/go-mp4's box writer for all ISO boxes.
func BuildFMP4(cfg Config, video []Fragment, audio *Fragment) (*Result, error) {
	if cfg.Width == 0 {
		cfg.Width = 320
	}
	if cfg.Height == 0 {
		cfg.Height = 180
	}
	if cfg.AudioChannels == 0 {
		cfg.AudioChannels = 2
	}
	sb := &seekBuffer{}
	w := mp4.NewWriter(sb)

	if err := writeFileType(w); err != nil {
		return nil, wrapMux(err, 0, "write ftyp")
	}
	if _, err := startBox(w, mp4.BoxTypeMoov()); err != nil {
		return nil, wrapMux(err, 0, "start moov")
	}
	if err := writeMovieHeader(w, cfg.DurationTicks); err != nil {
		return nil, wrapMux(err, 0, "write mvhd")
	}
	if err := writeTrack(w, 1, [4]byte{'v', 'i', 'd', 'e'}, "MediaCheck Video",
		cfg.DurationTicks, true, cfg.Width, cfg.Height, 0); err != nil {
		return nil, wrapMux(err, 0, "write video trak")
	}
	if err := writeTrack(w, 2, [4]byte{'s', 'o', 'u', 'n'}, "MediaCheck Audio",
		cfg.DurationTicks, false, 0, 0, cfg.AudioChannels); err != nil {
		return nil, wrapMux(err, 0, "write audio trak")
	}
	if _, err := startBox(w, mp4.BoxTypeMvex()); err != nil {
		return nil, wrapMux(err, 0, "start mvex")
	}
	if err := writeTrex(w, 1); err != nil {
		return nil, wrapMux(err, 0, "write video trex")
	}
	if err := writeTrex(w, 2); err != nil {
		return nil, wrapMux(err, 0, "write audio trex")
	}
	if err := endBox(w); err != nil { // mvex
		return nil, wrapMux(err, 0, "end mvex")
	}
	if err := endBox(w); err != nil { // moov
		return nil, wrapMux(err, 0, "end moov")
	}

	result := &Result{}
	sequence := uint32(1)
	for _, frag := range video {
		info, err := writeFragment(w, sb, 1, frag, sequence)
		if err != nil {
			return nil, err
		}
		sequence++
		result.Segments = append(result.Segments, info)
	}
	if audio != nil && len(audio.Samples) > 0 {
		if _, err := writeFragment(w, sb, 2, *audio, sequence); err != nil {
			return nil, err
		}
	}
	result.Bytes = append([]byte(nil), sb.Bytes()...)
	return result, nil
}

func writeFragment(w *mp4.Writer, sb *seekBuffer, trackID uint32,
	frag Fragment, sequence uint32) (SegmentInfo, error) {
	if len(frag.Samples) == 0 {
		return SegmentInfo{}, media.E(media.StageMux, media.KindInvalidArgument, -1,
			fmt.Errorf("fragment track=%d has no samples", trackID))
	}
	moofInfo, err := startBox(w, mp4.BoxTypeMoof())
	if err != nil {
		return SegmentInfo{}, wrapMux(err, 0, "start moof")
	}
	if err := marshalLeaf(w, &mp4.Mfhd{SequenceNumber: sequence}); err != nil {
		return SegmentInfo{}, wrapMux(err, int64(moofInfo.Offset), "mfhd")
	}
	if _, err := startBox(w, mp4.BoxTypeTraf()); err != nil {
		return SegmentInfo{}, wrapMux(err, int64(moofInfo.Offset), "start traf")
	}
	tfhd := &mp4.Tfhd{TrackID: trackID}
	tfhd.SetFlags(mp4.TfhdDefaultBaseIsMoof)
	if err := marshalLeaf(w, tfhd); err != nil {
		return SegmentInfo{}, wrapMux(err, int64(moofInfo.Offset), "tfhd")
	}
	tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV1: uint64(frag.BasePTS)}
	tfdt.SetVersion(1)
	if err := marshalLeaf(w, tfdt); err != nil {
		return SegmentInfo{}, wrapMux(err, int64(moofInfo.Offset), "tfdt")
	}

	trun := &mp4.Trun{SampleCount: uint32(len(frag.Samples))}
	trun.SetFlags(0x000001 | 0x000100 | 0x000200) // data-offset, duration, size
	for _, s := range frag.Samples {
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration: uint32(s.Duration),
			SampleSize:     uint32(len(s.Data)),
		})
	}
	trunInfo, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrun()})
	if err != nil {
		return SegmentInfo{}, wrapMux(err, 0, "start trun")
	}
	if _, err := mp4.Marshal(w, trun, mp4.Context{}); err != nil {
		return SegmentInfo{}, wrapMux(err, int64(trunInfo.Offset), "marshal trun")
	}
	if err := endBox(w); err != nil {
		return SegmentInfo{}, wrapMux(err, int64(trunInfo.Offset), "end trun")
	}
	if err := endBox(w); err != nil { // traf
		return SegmentInfo{}, wrapMux(err, int64(moofInfo.Offset), "end traf")
	}
	moofEnd, err := w.EndBox() // moof
	if err != nil {
		return SegmentInfo{}, wrapMux(err, int64(moofInfo.Offset), "end moof")
	}

	dataOffset := int32(moofEnd.Size + 8) // mdat short header
	patchAt := int64(trunInfo.Offset) + int64(trunInfo.HeaderSize) + 8
	binary.BigEndian.PutUint32(sb.b[patchAt:patchAt+4], uint32(dataOffset))

	mdatStart, err := startBox(w, mp4.BoxTypeMdat())
	if err != nil {
		return SegmentInfo{}, wrapMux(err, int64(moofEnd.Offset), "start mdat")
	}
	payloadBytes := 0
	var duration int64
	for _, s := range frag.Samples {
		if _, err := w.Write(s.Data); err != nil {
			return SegmentInfo{}, wrapMux(err, int64(mdatStart.Offset), "write mdat")
		}
		payloadBytes += len(s.Data)
		duration += s.Duration
	}
	mdatEnd, err := w.EndBox()
	if err != nil {
		return SegmentInfo{}, wrapMux(err, int64(mdatStart.Offset), "end mdat")
	}
	total := int(mdatEnd.Offset + mdatEnd.Size - moofEnd.Offset)
	return SegmentInfo{
		BasePTS: frag.BasePTS, DurationTicks: duration, Bytes: total,
	}, nil
}

func wrapMux(err error, offset int64, msg string) error {
	if me, ok := media.AsError(err); ok {
		return me
	}
	return media.E(media.StageMux, media.KindValidation, offset, fmt.Errorf("%s: %w", msg, err))
}

// AnnexBToLengthPrefixed converts one Annex-B NAL payload into MP4 4-byte
// length format. The input excludes the start code but includes NAL header.
func AnnexBToLengthPrefixed(nalPayload []byte) []byte {
	out := make([]byte, 4+len(nalPayload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(nalPayload)))
	copy(out[4:], nalPayload)
	return out
}
