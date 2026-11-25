package recovery

import (
	"os"

	"mediad/internal/audio"
)

func (e *exec) stageLoudness() error {
	name := "master.wav"
	path := e.artifactPath(name)
	wantHash := e.cp.Artifacts["master_wav"]

	if e.cp.StageDone("loudness") && wantHash != "" {
		if data, err := os.ReadFile(path); err == nil && HashBytes(data) == wantHash {
			e.master = data
			buf, err := audio.DecodeWAV(data)
			if err != nil {
				return err
			}
			report := audio.Measure(buf)
			e.sum.IntegratedLUFS = report.IntegratedLUFS
			e.sum.TruePeakDBTP = report.TruePeakDBTP
			return e.finishStage("loudness")
		}
	}

	samples := decodePCM16LE(e.pcm)
	buf, err := audio.PCM16ToBuffer(samples, e.params.Rate, 2)
	if err != nil {
		return err
	}
	mastered, report, err := audio.Master(buf, e.params.LUFS, -1.0)
	if err != nil {
		return err
	}
	wav, err := audio.EncodeWAV(mastered)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, wav); err != nil {
		return err
	}
	e.master = wav
	if e.cp.Artifacts == nil {
		e.cp.Artifacts = map[string]string{}
	}
	e.cp.Artifacts["master_wav"] = HashBytes(wav)
	e.sum.IntegratedLUFS = report.IntegratedLUFS
	e.sum.TruePeakDBTP = report.TruePeakDBTP
	return e.finishStage("loudness")
}
