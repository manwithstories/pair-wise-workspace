package rtp

import (
	"mediad/internal/clock"
	"mediad/internal/model"
)

// Track converts delivered packets into a track for the repair stage. DTS is
// derived from the raw 32-bit RTP timestamp, so a timestamp wrap is handed to
// repair intact instead of being hidden here. Missing packets become Damaged
// samples.
func (r *Receiver) Track(id uint32, packets []Packet) (model.Track, error) {
	tr := model.Track{ID: id, Kind: model.Video, Codec: "rtp", Source: "rtp", Timescale: r.cfg.ClockRate}
	frameDur := clock.Zero()
	if r.cfg.ClockRate == 0 {
		return tr, model.New(model.KindInvalidInput, "rtp.reorder", "zero RTP clock rate")
	}
	prevTick := int64(-1)
	for _, p := range packets {
		dts, err := clock.New(int64(p.Timestamp), int64(r.cfg.ClockRate))
		if err != nil {
			return tr, model.Wrap(model.KindInvalidMedia, "rtp.reorder", "packet timestamp", err)
		}
		if prevTick >= 0 {
			if d, err := clock.New(int64(p.Timestamp)-prevTick, int64(r.cfg.ClockRate)); err == nil && d.Sign() > 0 {
				frameDur = d
			}
		}
		if frameDur.IsZero() {
			frameDur, _ = clock.New(1, int64(r.cfg.ClockRate))
		}
		tr.Samples = append(tr.Samples, model.Sample{
			Index: len(tr.Samples), Offset: 0, Size: uint32(p.PayloadBytes),
			DTS: dts, PTS: dts, Duration: frameDur,
			Keyframe: true,
		})
		prevTick = int64(p.Timestamp)
	}
	return tr, nil
}
