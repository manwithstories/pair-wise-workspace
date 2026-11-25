package demux

import (
	"fmt"
	"io"

	"mediaarchive/internal/model"
)

// StreamOptions controls payload streaming.
type StreamOptions struct {
	// RecoverTruncated makes samples crossing EOF return a recoverable
	// model.Error with a zero-length payload instead of aborting.
	RecoverTruncated bool
}

// StreamSamples yields every sample in table order across all chunks. The
// callback may return an error to stop the walk. Payload slices are freshly
// allocated and owned by the caller. A truncated mdat produces either a
// recoverable error (recover mode) or a hard error; a bad index is always a
// hard error because the sample table itself is untrustworthy.
func (c *Container) StreamSamples(opts StreamOptions, fn func(Sample) error) error {
	for ci, table := range c.Samples {
		var chunkEnd int64
		if ci < len(c.Chunks) {
			chunkEnd = c.Chunks[ci].DataEnd
		}
		for si := range table {
			s := table[si]
			declaredEnd := s.Offset + int64(s.Size)
			if s.Offset < 0 || s.Size == 0 {
				return model.NewError(model.StageDemux,
					fmt.Sprintf("bad sample index: track=%d seq=%d offset=%d size=%d",
						s.TrackID, s.Seq, s.Offset, s.Size)).
					WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq)
			}
			if declaredEnd > chunkEnd {
				me := model.NewError(model.StageDemux,
					fmt.Sprintf("truncated mdat at track=%d seq=%d: need %d bytes to %d, data ends %d",
						s.TrackID, s.Seq, s.Size, declaredEnd, chunkEnd)).
					WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq).Recover()
				if !opts.RecoverTruncated {
					return me
				}
				s.Flags |= FlagTruncated
				s.Size = 0
				s.Payload = []byte{}
				c.recordDamage(s, me)
				if err := fn(s); err != nil {
					return err
				}
				continue
			}
			buf := make([]byte, s.Size)
			if _, err := c.r.Seek(s.Offset, io.SeekStart); err != nil {
				return model.NewError(model.StageDemux, "cannot seek sample payload").
					WithTrack(int32(s.TrackID)).WithOffset(s.Offset).Wrap(err)
			}
			if _, err := io.ReadFull(c.r, buf); err != nil {
				return model.NewError(model.StageDemux, "short read inside declared mdat").
					WithTrack(int32(s.TrackID)).WithOffset(s.Offset).
					WithSample(s.Seq).Recover().Wrap(err)
			}
			s.Payload = buf
			if err := fn(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// Damage records one recovered damage event for the manifest.
type Damage struct {
	Stage     string
	TrackID   int32
	Offset    int64
	SampleSeq uint32
	Reason    string
}

func (c *Container) recordDamage(s Sample, me *model.Error) {
	c.Damages = append(c.Damages, Damage{
		Stage:     me.Stage,
		TrackID:   int32(s.TrackID),
		Offset:    me.Offset,
		SampleSeq: s.Seq,
		Reason:    me.Reason,
	})
}

// AllTrackIDs returns the declared track ids in declaration order.
func (c *Container) AllTrackIDs() []uint32 {
	ids := make([]uint32, len(c.Tracks))
	for i, t := range c.Tracks {
		ids[i] = t.ID
	}
	return ids
}

// Track looks up track metadata by id.
func (c *Container) Track(id uint32) (TrackMeta, bool) {
	for _, t := range c.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return TrackMeta{}, false
}
