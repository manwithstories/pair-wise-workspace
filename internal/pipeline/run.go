// Package pipeline wires capture, reorder, PCM, DSP, transactions and MP4
// output into one resumable orchestration.
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"mediad/internal/capture"
	"mediad/internal/clock"
	"mediad/internal/dsp"
	"mediad/internal/fault"
	"mediad/internal/mp4"
	"mediad/internal/pcm"
	"mediad/internal/txn"
)

// Run executes or resumes the pipeline. A fault.Error with exit code 75 is
// returned when Config.InjectStage is reached.
func Run(cfg Config) (*Result, error) {
	if cfg.Rate == 0 {
		cfg.Rate = clock.Rate44100
	}
	if cfg.Channels == 0 {
		cfg.Channels = 6
	}
	if cfg.PacketFrames == 0 {
		cfg.PacketFrames = cfg.Rate / 100
	}
	if cfg.ExpectedDamageOffset == 0 {
		cfg.ExpectedDamageOffset = -1
	}
	if cfg.TxID == "" {
		cfg.TxID = txn.StableTxID(cfg.InputPath, cfg.OutputPath, fmt.Sprint(cfg.Rate), fmt.Sprint(cfg.Channels))
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = ".mediad-work"
	}

	inputInfo, err := os.Stat(cfg.InputPath)
	if err != nil {
		return nil, fault.Usage("input", err)
	}
	if err := fault.CheckInputSize(inputInfo.Size()); err != nil {
		return nil, fault.Usage("input", err)
	}

	m, err := txn.OpenManager(cfg.WorkDir, cfg.TxID, cfg.InjectStage)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	res := &Result{Rollbacks: m.Rollbacks, DamageOffset: -1, DamageOffsetError: -1}
	if cfg.IndexPath != "" {
		_, rebuilt, err := capture.LoadOrRebuild(cfg.IndexPath, cfg.InputPath, 32)
		if err != nil {
			return res, fault.Unrecoverable("index", err)
		}
		res.IndexRebuilt = rebuilt
	}

	decoded, err := decodeStage(cfg, m, res)
	if err != nil {
		return res, err
	}
	master, err := dspStage(m, res, decoded)
	if err != nil {
		return res, err
	}
	res.Samples = master.Frames

	if cached, ok := committedMP4(cfg, m, res); ok {
		return cached, nil
	}
	return renderAndCommitMP4(cfg, m, res, master)
}

func decodeStage(cfg Config, m *txn.Manager, res *Result) (*pcm.Audio, error) {
	if m.IsCommitted("decode") {
		decoded, err := loadL16(m.Path("decoded.l16"), cfg.Rate, cfg.Channels)
		if err != nil {
			return nil, fault.Unrecoverable("decode-cache", err)
		}
		loadCaptureReport(m, res)
		return decoded, nil
	}

	report, blocks, err := scanAndReorder(cfg, res)
	if err != nil {
		return nil, err
	}
	if err := commitJSON(m, "capture", "capture.json", report); err != nil {
		return nil, err
	}
	audio, missingFrames, err := pcm.DecodeBlocks(blocks, cfg.Rate, cfg.Channels)
	if err != nil {
		return nil, fault.Unrecoverable("decode", err)
	}
	res.MissingFrames = missingFrames
	if _, err := m.CommitStage("decode", "decoded.l16", audio.ToS16BE()); err != nil {
		return nil, err
	}
	return audio, nil
}

func dspStage(m *txn.Manager, res *Result, decoded *pcm.Audio) (*pcm.Audio, error) {
	if m.IsCommitted("dsp") {
		master, err := loadL16(m.Path("master.l16"), clock.Rate48000, 2)
		if err != nil {
			return nil, fault.Unrecoverable("dsp-cache", err)
		}
		var lr dsp.LoudnessReport
		if b, err := os.ReadFile(m.Path("dsp-report.json")); err == nil {
			_ = json.Unmarshal(b, &lr)
		}
		res.Loudness = lr
		return master, nil
	}

	stereo, err := dsp.NormalizeLayout(decoded)
	if err != nil {
		return nil, fault.Unrecoverable("layout", err)
	}
	master48, err := dsp.Resample(stereo, clock.Rate48000)
	if err != nil {
		return nil, fault.Unrecoverable("resample", err)
	}
	limited, lr, err := dsp.ProcessLoudness(master48)
	if err != nil {
		return nil, fault.Unrecoverable("loudness", err)
	}
	res.Loudness = lr
	if _, err := m.CommitStage("dsp", "master.l16", limited.ToS16BE()); err != nil {
		return nil, err
	}
	if err := commitJSON(m, "dsp-report", "dsp-report.json", lr); err != nil {
		return nil, err
	}
	return limited, nil
}

func committedMP4(cfg Config, m *txn.Manager, res *Result) (*Result, bool) {
	if cfg.OutputPath == "" || !m.IsCommitted("mp4") {
		return nil, false
	}
	st := m.CP.Stages["mp4"]
	sum, n, err := txn.SHA256File(cfg.OutputPath)
	if err != nil || sum != st.SHA256 || n != st.Bytes {
		return nil, false
	}
	res.SHA256 = sum
	res.Bytes = n
	return res, true
}

func renderAndCommitMP4(cfg Config, m *txn.Manager, res *Result, master *pcm.Audio) (*Result, error) {
	mp4Bytes, _, err := mp4.Render(master, mp4.DefaultFragmentFrames)
	if err != nil {
		return res, fault.Unrecoverable("mp4-render", err)
	}
	out := cfg.OutputPath
	if out == "" {
		out = m.Path("final.mp4")
	}
	st, err := m.CommitStage("mp4", out, mp4Bytes)
	if err != nil {
		var fe *fault.Error
		if errors.As(err, &fe) && fe.Code == fault.ExitInterrupted {
			res.InjectedExit = true
		}
		return res, err
	}
	res.SHA256 = st.SHA256
	res.Bytes = st.Bytes
	return res, nil
}
