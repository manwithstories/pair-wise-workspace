package demux

import (
	"encoding/binary"
	"io"

	"mediacore/internal/clock"
	"mediacore/internal/fault"
	"mediacore/internal/sample"
)

// rebuildIndex recovers a track whose chunk-offset box is absent by scanning
// the mdat for self-describing frames and matching them against stsz sizes.
func (r *Reader) rebuildIndex(meta *TrackMeta, mdatOff int64) *fault.Fault {
	frames, f := r.scanMdatFrames(mdatOff)
	if f != nil {
		return f.WithTrack(0, meta.Index)
	}
	meta.chunkOff = make([]int64, 0, meta.Samples)
	matched := 0
	for _, fr := range frames {
		if fr.trackID != meta.TrackID {
			continue
		}
		if matched >= meta.Samples {
			break
		}
		// stsz sizes include the 16-byte framing header.
		if uint32(fr.length)+sample.FrameHeaderLen != meta.sizes[matched] {
			return fault.New(fault.KindCorruptSample, fault.Abort, fr.offset,
				"rebuilt frame length disagrees with stsz").WithTrack(0, meta.Index)
		}
		meta.chunkOff = append(meta.chunkOff, fr.offset)
		matched++
	}
	if matched != meta.Samples {
		return fault.New(fault.KindMissingIndex, fault.Abort, mdatOff,
			"index reconstruction found too few frames").WithTrack(0, meta.Index)
	}
	return nil
}

type mdatFrame struct {
	offset  int64
	trackID uint32
	length  int
}

// scanMdatFrames walks every framed sample in mdat, stopping at the first
// frame whose header claims bytes past EOF (truncated tail).
func (r *Reader) scanMdatFrames(mdatPayloadOff int64) ([]mdatFrame, *fault.Fault) {
	pos := mdatPayloadOff
	end := r.mdatEnd
	if end <= 0 || end > r.fileSz {
		end = r.fileSz
	}
	var out []mdatFrame
	for pos < end {
		hdr := make([]byte, sample.FrameHeaderLen)
		if _, err := r.rs.Seek(pos, io.SeekStart); err != nil {
			return out, fault.New(fault.KindCorruptSample, fault.Abort, pos, err.Error())
		}
		n, err := io.ReadFull(r.rs, hdr)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return out, fault.New(fault.KindTruncatedHeader, fault.Abort,
				pos+int64(n), "frame header truncated")
		}
		if err != nil {
			return out, fault.New(fault.KindCorruptSample, fault.Abort, pos, err.Error())
		}
		// Parse the 16-byte header directly: ParseFrame requires the whole frame.
		var magic [4]byte
		copy(magic[:], hdr[0:4])
		if magic != sample.FrameMagic {
			return out, fault.New(fault.KindCorruptSample, fault.SkipFragment,
				pos, "invalid frame magic in mdat scan")
		}
		length := int(hdr[7]) | int(hdr[6])<<8 | int(hdr[5])<<16 | int(hdr[4])<<24
		trackID := binary.BigEndian.Uint32(hdr[8:12])
		next := pos + int64(sample.FrameHeaderLen+length)
		if next > end {
			return out, fault.New(fault.KindTruncatedHeader, fault.Abort,
				pos+int64(sample.FrameHeaderLen), "frame payload truncated")
		}
		out = append(out, mdatFrame{offset: pos, trackID: trackID, length: length})
		pos = next
	}
	return out, nil
}

// ReadSamples iterates one track's samples, validating each frame CRC and
// converting timing into unified ticks. Recoverable faults are delivered via
// onFault while the loop continues at the next whole sample.
func (r *Reader) ReadSamples(meta *TrackMeta, mdatPayloadOff int64,
	onFault func(*fault.Fault)) ([]*sample.Sample, error) {

	if len(meta.chunkOff) != meta.Samples {
		return nil, fault.New(fault.KindMissingIndex, fault.Abort, mdatPayloadOff,
			"track offsets not resolved").WithTrack(meta.TrackID, meta.Index)
	}
	out := make([]*sample.Sample, 0, meta.Samples)
	var timeUnits uint64
	runIdx, withinRun := 0, uint32(0)
	for i := 0; i < meta.Samples; i++ {
		delta := meta.sttsRuns[runIdx].delta
		off := meta.chunkOff[i]
		size := int(meta.sizes[i])
		buf := make([]byte, size)
		if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		n, err := io.ReadFull(r.rs, buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			onFault(fault.New(fault.KindTruncatedHeader, fault.SkipFragment,
				off+int64(n), "sample truncated; fragment skipped").
				WithTrack(meta.TrackID, meta.Index))
			break
		}
		if err != nil {
			return nil, err
		}
		trackID, payload, bad, ok := sample.ParseFrame(buf)
		if ok {
			pts := clock.FromTimescale(timeUnits, meta.Timescale)
			dur := clock.FromTimescale(uint64(delta), meta.Timescale)
			out = append(out, &sample.Sample{
				TrackIndex: meta.Index, TrackID: trackID, Codec: meta.Codec,
				Seq: i, PTS: pts, Duration: dur, Offset: off,
				Data: append([]byte(nil), payload...), Keyframe: meta.Keyframes[i],
			})
		} else {
			onFault(fault.New(fault.KindCorruptSample, fault.SkipFragment,
				off+int64(bad), "frame check failed; sample skipped").
				WithTrack(meta.TrackID, meta.Index))
		}
		// Advance timing regardless so later PTS stays aligned.
		timeUnits += uint64(delta)
		withinRun++
		if withinRun >= meta.sttsRuns[runIdx].count {
			withinRun = 0
			runIdx++
		}
	}
	return out, nil
}

func (m *TrackMeta) deltaAt(idx int) uint32 {
	if len(m.sttsRuns) == 0 {
		return 0
	}
	if idx >= len(m.sttsRuns) {
		return m.sttsRuns[len(m.sttsRuns)-1].delta
	}
	return m.sttsRuns[idx].delta
}

// ReadAll drains every track and returns per-track samples plus all
// recoverable faults. Structural errors are returned as a hard error.
func (c *Container) ReadAll() ([][]*sample.Sample, []*fault.Fault, error) {
	var recovered []*fault.Fault
	out := make([][]*sample.Sample, len(c.Tracks))
	for i, meta := range c.Tracks {
		samples, err := c.Reader.ReadSamples(meta, c.Reader.mdatPayload, func(f *fault.Fault) {
			recovered = append(recovered, f)
		})
		if err != nil {
			return nil, nil, err
		}
		out[i] = samples
	}
	return out, recovered, nil
}
