package pipeline

import (
	"math"

	"mediarig/internal/audio"
	"mediarig/internal/media"
	"mediarig/internal/txn"
)

// BuildNormalizedAudio restores missing audio as exact silence, downmixes and
// resamples, then performs loudness/true-peak normalization.
func BuildNormalizedAudio(cap *CaptureResult, p txn.Params) ([]byte, audio.LoudnessReport, error) {
	layout, err := audio.ParseLayout(p.Layout)
	if err != nil {
		return nil, audio.LoudnessReport{}, err
	}
	ch := layout.Channels()
	tr := cap.Audio
	gapAfter := map[uint64]int{}
	fallback := p.Rate / 50
	for _, g := range tr.Gaps {
		n := tr.GapSamples(g)
		if n <= 0 {
			n = fallback * g.Count
		}
		gapAfter[g.PrevSeq] += n
	}

	channels := make([][]float64, ch)
	for _, pkt := range tr.Packets {
		dec, err := audio.DecodeL16BE(pkt.Packet.Payload, ch)
		if err != nil {
			cerr := media.CorruptWrap(media.StageAudio, err, "decode L16")
			cerr = media.WithPacketSequence(cerr, pkt.Sequence())
			return nil, audio.LoudnessReport{}, cerr
		}
		for c := 0; c < ch; c++ {
			channels[c] = append(channels[c], dec.Channels[c]...)
		}
		if silence := gapAfter[pkt.ExtSequence]; silence > 0 {
			for c := 0; c < ch; c++ {
				channels[c] = append(channels[c], make([]float64, silence)...)
			}
		}
	}
	raw := audio.Audio{SampleRate: p.Rate, Layout: layout, Channels: channels}
	mixed, err := audio.Downmix(raw, layout)
	if err != nil {
		return nil, audio.LoudnessReport{}, err
	}
	out48, err := audio.Resample(mixed, 48000)
	if err != nil {
		return nil, audio.LoudnessReport{}, err
	}
	target := p.LUFS
	if target == 0 {
		target = -23
	}
	normalized, loud, err := audio.NormalizeToBroadcast(out48, target)
	if err != nil {
		return nil, loud, err
	}
	return audio.EncodeS16LE(normalized), loud, nil
}

// ActualPCMBitrateKbps derives bitrate from integer PCM byte duration.
func ActualPCMBitrateKbps(pcm []byte) float64 {
	frameBytes := 2 * 2 // stereo × 16-bit
	frames := len(pcm) / frameBytes
	seconds := float64(frames) / 48000.0
	if seconds == 0 {
		return 0
	}
	return float64(len(pcm)*8) / seconds / 1000.0
}

func deviationPct(actual, target float64) float64 {
	if target == 0 {
		return 0
	}
	return math.Abs(actual-target) / target * 100
}
