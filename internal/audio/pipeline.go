package audio

// NormalizePipeline runs the full audio normalization chain: resample to
// 48 kHz, downmix the source layout to stereo with an explicit channel map,
// then normalize loudness and true-peak. LFEPeak reports the discrete LFE
// level that is not folded into the stereo bus.
func NormalizePipeline(frames []*Frame) (stereo48 []float32, lfePeak float32, stats LoudnessStats, err error) {
	rs := NewResampler(2)
	var downmixed []float32
	for _, f := range frames {
		stereo, peak, derr := Downmix(f)
		if derr != nil {
			return nil, 0, stats, derr
		}
		if peak > lfePeak {
			lfePeak = peak
		}
		conv, rerr := rs.Process(stereo, f.SampleRate, 2)
		if rerr != nil {
			return nil, 0, stats, rerr
		}
		downmixed = append(downmixed, conv...)
	}
	out, st := Normalize(downmixed, TargetRate)
	return out, lfePeak, st, nil
}
