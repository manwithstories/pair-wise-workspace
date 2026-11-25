package bmff

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"avlab/internal/media"
)

// Keyframe points at one independently decodable sample.
type Keyframe struct {
	Segment  uint32
	Tick     media.MediaTime
	Size     uint32
	IsFirst  bool
}

// SampleIndex is the per-sample projection of a trun box.
type SampleIndex struct {
	Tick     media.MediaTime
	Duration media.MediaTime
	Size     uint32
	Keyframe bool
}

// SegmentIndex is the deterministic index of one fMP4 segment.
type SegmentIndex struct {
	Sequence  uint32
	TrackID   uint32
	Epoch     media.Epoch
	BaseTime  media.MediaTime
	Samples   []SampleIndex
	Keyframes []Keyframe
}

// IndexFragment parses an fMP4 segment. Rejected inputs: boxes extending past
// the buffer, truncated payloads and timestamp computations that overflow.
func IndexFragment(data []byte, epoch media.Epoch) (*SegmentIndex, error) {
	if len(data) == 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, false, "empty fragment")
	}
	if len(data) > media.MaxSegmentBytes {
		return nil, media.NewFault(media.CodeSegmentTooLarge, false,
			fmt.Sprintf("fragment %d bytes exceeds limit %d",
				len(data), media.MaxSegmentBytes))
	}
	idx := &SegmentIndex{Epoch: epoch}
	r := bytes.NewReader(data)
	var trun *mp4.Trun
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (any, error) {
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, classifyMp4Error(err)
		}
		switch b := box.(type) {
		case *mp4.Mfhd:
			idx.Sequence = b.SequenceNumber
		case *mp4.Tfhd:
			idx.TrackID = b.TrackID
		case *mp4.Tfdt:
			base := b.GetBaseMediaDecodeTime()
			if base > uint64(media.MaxTick) {
				return nil, media.NewFault(media.CodeTimestampOverflow, false,
					"tfdt base media decode time exceeds int64 range")
			}
			idx.BaseTime = media.MediaTime(base)
		case *mp4.Trun:
			trun = b
		}
		// recurse into container boxes (moof) and skip unknown leaf boxes
		return h.Expand()
	})
	if err != nil {
		return nil, classifyMp4Error(err)
	}
	if trun == nil {
		return nil, media.NewFault(media.CodeBoxTruncated, false,
			"moof contains no trun box")
	}
	if err := idx.projectSamples(trun); err != nil {
		return nil, err
	}
	if err := idx.checkMdatBounds(r, data); err != nil {
		return nil, err
	}
	return idx, nil
}

func (idx *SegmentIndex) projectSamples(trun *mp4.Trun) error {
	if trun.SampleCount == 0 || int(trun.SampleCount) != len(trun.Entries) {
		return media.NewFault(media.CodeBoxTruncated, false,
			"trun sample count does not match entry count")
	}
	cur := idx.BaseTime
	for i, e := range trun.Entries {
		dur := media.MediaTime(e.SampleDuration)
		next, err := cur.Add(dur)
		if err != nil {
			return media.WrapFault(media.CodeTimestampOverflow, false,
				fmt.Sprintf("sample %d timestamp overflow", i), err)
		}
		// ISO 14496-12: sample_depends_on=1 means "does not depend on
		// others"; the writer marks non-sync samples via 0x02000000.
		keyframe := e.SampleFlags&0x01000000 != 0
		si := SampleIndex{
			Tick: cur, Duration: dur, Size: e.SampleSize, Keyframe: keyframe,
		}
		idx.Samples = append(idx.Samples, si)
		if keyframe {
			idx.Keyframes = append(idx.Keyframes, Keyframe{
				Segment: idx.Sequence, Tick: cur, Size: e.SampleSize,
				IsFirst: i == 0,
			})
		}
		cur = next
	}
	return nil
}

// checkMdatBounds verifies that the mdat box declared size covers exactly the
// bytes remaining in the segment and every trun sample fits inside it.
func (idx *SegmentIndex) checkMdatBounds(r io.ReadSeeker, data []byte) error {
	boxes, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil {
		return classifyMp4Error(err)
	}
	if len(boxes) == 0 {
		return media.NewFault(media.CodeBoxTruncated, false, "missing mdat box")
	}
	bi := boxes[0]
	if bi.Offset+bi.Size > uint64(len(data)) {
		return media.NewFault(media.CodeBoxOutOfBounds, false,
			fmt.Sprintf("mdat extends past segment: end=%d size=%d",
				bi.Offset+bi.Size, len(data)))
	}
	var total uint64
	for _, s := range idx.Samples {
		total += uint64(s.Size)
	}
	payloadCap := bi.Size - bi.HeaderSize
	if total > payloadCap {
		return media.NewFault(media.CodeBoxOutOfBounds, false,
			fmt.Sprintf("trun samples need %d bytes but mdat holds %d",
				total, payloadCap))
	}
	return nil
}

// classifyMp4Error maps go-mp4 errors onto the avlab Fault taxonomy.
func classifyMp4Error(err error) error {
	if media.AsFault(err) != nil {
		return err
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "exceeds file size", "too large box", "unexpected EOF"):
		return media.WrapFault(media.CodeBoxOutOfBounds, true,
			"box boundary violation", err)
	case containsAny(msg, "io: EOF", "EOF", "short"):
		return media.WrapFault(media.CodeBoxTruncated, true,
			"truncated box payload", err)
	default:
		return media.WrapFault(media.CodeBoxTruncated, true,
			"malformed fMP4 structure", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}
