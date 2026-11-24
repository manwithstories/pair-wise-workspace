package pipeline

import (
	"encoding/json"
	"os"

	"mediad/internal/pcm"
	"mediad/internal/txn"
)

func loadL16(path string, rate, channels int) (*pcm.Audio, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	samples, frames, err := pcm.DecodeL16(b, channels)
	if err != nil {
		return nil, err
	}
	return &pcm.Audio{SampleRate: rate, Channels: channels, Frames: frames, Samples: samples}, nil
}

func commitJSON(m *txn.Manager, stage, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = m.CommitStage(stage, name, b)
	return err
}

func loadCaptureReport(m *txn.Manager, res *Result) {
	b, err := os.ReadFile(m.Path("capture.json"))
	if err != nil {
		return
	}
	var r CaptureReport
	if err := json.Unmarshal(b, &r); err != nil {
		return
	}
	res.Records = r.Records
	res.IndexRebuilt = r.IndexRebuilt
	res.DamageOffset = r.DamageOffset
	res.DamageOffsetError = r.DamageOffsetError
	res.Reorder = r.Stats
	res.MissingFrames = r.MissingFrames
}
