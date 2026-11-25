package recovery

import (
	"math"

	"mediad/internal/clock"
	"mediad/internal/codec"
	"mediad/internal/fault"
)

func (e *exec) stageRTP() error {
	mapper := codec.NewRTPMapper(e.opts.RTP.ClockRate, e.opts.RTP.BaseSequence, e.opts.RTP.BaseTimestamp)
	frame := 0
	var last codec.MappedPacket
	for _, seg := range e.segs {
		for _, n := range seg.nals {
			wireSeq := e.opts.RTP.BaseSequence + uint16(frame)
			rawTS := int64(e.opts.RTP.BaseTimestamp) + int64(frame)*e.opts.RTP.FrameTicks
			wireTS := uint32(rawTS & 0xffffffff)
			mp, err := mapper.Map(wireSeq, wireTS, n.IDR, nil)
			if err != nil {
				return err
			}
			want, err := clock.New(int64(frame)*e.opts.RTP.FrameTicks, e.opts.RTP.ClockRate)
			if err != nil {
				return err
			}
			if clock.Cmp(mp.MediaTime, want) != 0 {
				return fault.New(fault.TimestampBackward, seg.path, n.Offset,
					"RTP wraparound changed media time: got %s want %s", mp.MediaTime, want)
			}
			if mp.ExtSequence > math.MaxUint16 || mp.ExtTimestamp > math.MaxUint32 {
				e.sum.RTPWrapped = true
			}
			last = mp
			e.maps = append(e.maps, mp)
			frame++
		}
	}
	e.sum.RTPExtSequence = last.ExtSequence
	e.sum.RTPExtTimestamp = last.ExtTimestamp
	return e.finishStage("rtp")
}
