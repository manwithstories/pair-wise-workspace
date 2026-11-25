package pipeline

import (
	"encoding/json"

	"fieldcut/internal/audio"
	"fieldcut/internal/clock"
)

func stageInputHashes(b *Bundle) map[string]string {
	ingestHash := HashBytes(append(append(append([]byte{}, b.DirectorBytes...), b.StageBytes...), b.Audio48Stereo...))
	audioHash := HashBytes(append(append(append([]byte{}, b.Audio48Stereo...), b.Audio44Mono...), b.Audio51...))
	eventBytes, _ := json.Marshal(b.RawEvents)
	alignHash := HashBytes(eventBytes)
	cutHash := HashBytes([]byte(alignHash + audioHash))
	return map[string]string{
		"generate":    b.RequestHash,
		"ingest":      ingestHash,
		"align":       alignHash,
		"graph":       ingestHash,
		"audio":       audioHash,
		"cut":         cutHash,
		"fingerprint": cutHash,
		"package":     HashBytes([]byte(cutHash + "package-v1")),
		"manifest":    HashBytes([]byte(cutHash + "manifest-v1")),
	}
}

func processAudio(b *Bundle) (*audio.PCM, audio.Metrics, []clock.Interval, map[string]string, error) {
	p48, err := audio.ReadPCM16(10, OutputRate, audio.LayoutStereo, b.Audio48Stereo)
	if err != nil {
		return nil, audio.Metrics{}, nil, nil, err
	}
	p44, err := audio.ReadPCM16(11, 44100, audio.LayoutMono, b.Audio44Mono)
	if err != nil {
		return nil, audio.Metrics{}, nil, nil, err
	}
	p51, err := audio.ReadPCM16(12, OutputRate, audio.Layout51, b.Audio51)
	if err != nil {
		return nil, audio.Metrics{}, nil, nil, err
	}
	out, met, silence, err := audio.Process(p48, OutputRate, audio.LayoutStereo, -45, 100*clock.NsPerMs)
	if err != nil {
		return nil, met, nil, nil, err
	}
	if _, _, _, err := audio.Process(p44, OutputRate, audio.LayoutStereo, -45, 100*clock.NsPerMs); err != nil {
		return nil, met, nil, nil, err
	}
	if _, _, _, err := audio.Process(p51, OutputRate, audio.LayoutStereo, -45, 100*clock.NsPerMs); err != nil {
		return nil, met, nil, nil, err
	}
	reports := map[string]string{
		"track-10-rms-dbfs": fmtFloat(met.RMSDBFS),
		"track-11":          "resampled-44100-mono-to-48000-stereo",
		"track-12":          "mapped-5.1-to-stereo",
	}
	return out, met, silence, reports, nil
}
