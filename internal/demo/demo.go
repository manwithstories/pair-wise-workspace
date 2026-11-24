// Package demo generates the bundled normal, corrupt and interruption cases
// and drives them through the real pipeline.
package demo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediad/internal/capture"
	"mediad/internal/fault"
	"mediad/internal/pipeline"
	"mediad/internal/sample"
	"mediad/internal/txn"
)

const CanonicalSamples = 576000 // 12 s * 48 kHz

type Result struct {
	Out               string
	RootMP4           string
	RootSHA256        string
	RerunSHA256       string
	ResumeMS          int64
	Normal            *pipeline.Result
	Corrupt           *pipeline.Result
	InterruptedResume *pipeline.Result
	NormalManifest    *sample.Manifest
	CorruptManifest   *sample.Manifest
	InterruptManifest *sample.Manifest
}

func Run(seed int64, seconds int, out string) (*Result, error) {
	if out == "" {
		return nil, fault.Usage("demo", errors.New("--out is required"))
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, fault.Usage("demo", err)
	}

	normalDir := filepath.Join(out, "normal")
	corruptDir := filepath.Join(out, "corrupt")
	interruptDir := filepath.Join(out, "interrupted")

	normalManifest, err := sample.Generate(sample.DefaultOptions(seed, seconds, normalDir, false))
	if err != nil {
		return nil, fault.Unrecoverable("generate-normal", err)
	}
	corruptManifest, err := sample.Generate(sample.DefaultOptions(seed, seconds, corruptDir, true))
	if err != nil {
		return nil, fault.Unrecoverable("generate-corrupt", err)
	}

	normalRes, err := pipeline.Run(pipeline.Config{
		InputPath: normalManifest.CapturePath, IndexPath: normalManifest.IndexPath,
		OutputPath: filepath.Join(normalDir, "final.mp4"), WorkDir: filepath.Join(normalDir, "work"),
		Rate: normalManifest.Rate, Channels: normalManifest.Channels, PacketFrames: normalManifest.PacketFrames,
	})
	if err != nil {
		return nil, err
	}
	if normalRes.Samples != seconds*48000 {
		return nil, fault.Unrecoverable("demo", fmt.Errorf("normal samples=%d want %d", normalRes.Samples, seconds*48000))
	}

	if err := verifyBadIndex(corruptManifest); err != nil {
		return nil, err
	}
	corruptIndex, err := workingBadIndex(corruptManifest)
	if err != nil {
		return nil, err
	}
	corruptRes, err := pipeline.Run(pipeline.Config{
		InputPath: corruptManifest.CapturePath, IndexPath: corruptIndex,
		OutputPath: filepath.Join(corruptDir, "final.mp4"), WorkDir: filepath.Join(corruptDir, "work"),
		Rate: corruptManifest.Rate, Channels: corruptManifest.Channels, PacketFrames: corruptManifest.PacketFrames,
		ExpectedDamageOffset: corruptManifest.BadCRCOffset,
	})
	if err != nil {
		return nil, err
	}
	if corruptRes.DamageOffsetError > 8 {
		return nil, fault.Unrecoverable("demo", fmt.Errorf("damage offset error %d > 8", corruptRes.DamageOffsetError))
	}
	if corruptRes.Samples != seconds*48000 {
		return nil, fault.Unrecoverable("demo", fmt.Errorf("corrupt samples=%d want %d", corruptRes.Samples, seconds*48000))
	}

	// Recreate the interruption sample so every demo invocation deterministically
	// demonstrates one torn transaction and one rollback.
	if err := os.RemoveAll(interruptDir); err != nil {
		return nil, err
	}
	interruptManifest, err := sample.Generate(sample.DefaultOptions(seed, seconds, interruptDir, true))
	if err != nil {
		return nil, fault.Unrecoverable("generate-interrupt", err)
	}
	interruptIndex, err := workingBadIndex(interruptManifest)
	if err != nil {
		return nil, err
	}
	interruptMP4 := filepath.Join(interruptDir, "final.mp4")
	interruptWork := filepath.Join(interruptDir, "work")
	baseCfg := pipeline.Config{
		InputPath: interruptManifest.CapturePath, IndexPath: interruptIndex,
		OutputPath: interruptMP4, WorkDir: interruptWork,
		Rate: interruptManifest.Rate, Channels: interruptManifest.Channels,
		PacketFrames:         interruptManifest.PacketFrames,
		ExpectedDamageOffset: interruptManifest.BadCRCOffset,
	}

	injectCfg := baseCfg
	injectCfg.InjectStage = "mp4"
	if _, err := pipeline.Run(injectCfg); fault.ExitCode(err) != fault.ExitInterrupted {
		if err == nil {
			return nil, fault.Unrecoverable("demo", errors.New("injected run unexpectedly completed"))
		}
		return nil, err
	}
	if _, err := os.Stat(interruptMP4); !errors.Is(err, os.ErrNotExist) {
		return nil, fault.Unrecoverable("demo", errors.New("interrupted write unexpectedly created final MP4"))
	}

	start := time.Now()
	resumeRes, err := pipeline.Run(baseCfg)
	resumeMS := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	if resumeRes.Rollbacks != 1 {
		return nil, fault.Unrecoverable("demo", fmt.Errorf("rollback=%d want 1", resumeRes.Rollbacks))
	}
	if resumeRes.Samples != CanonicalSamples {
		return nil, fault.Unrecoverable("demo", fmt.Errorf("samples=%d want %d", resumeRes.Samples, CanonicalSamples))
	}
	if resumeRes.DamageOffsetError > 8 {
		return nil, fault.Unrecoverable("demo", fmt.Errorf("resume damage offset error %d > 8", resumeRes.DamageOffsetError))
	}

	rerunRes, err := pipeline.Run(baseCfg)
	if err != nil {
		return nil, err
	}
	if rerunRes.SHA256 != resumeRes.SHA256 {
		return nil, fault.Unrecoverable("demo", errors.New("idempotent pipeline rerun changed MP4 checksum"))
	}

	rootMP4 := filepath.Join(out, "final.mp4")
	b, err := os.ReadFile(interruptMP4)
	if err != nil {
		return nil, err
	}
	if err := txn.AtomicWrite(rootMP4, b); err != nil {
		return nil, err
	}
	rootSHA, _, err := txn.SHA256File(rootMP4)
	if err != nil {
		return nil, err
	}
	if rootSHA != resumeRes.SHA256 {
		return nil, fault.Unrecoverable("demo", errors.New("root MP4 checksum does not match recovered fragment"))
	}

	return &Result{
		Out: out, RootMP4: rootMP4, RootSHA256: rootSHA, RerunSHA256: rerunRes.SHA256,
		ResumeMS: resumeMS, Normal: normalRes, Corrupt: corruptRes,
		InterruptedResume: resumeRes, NormalManifest: normalManifest,
		CorruptManifest: corruptManifest, InterruptManifest: interruptManifest,
	}, nil
}

func verifyBadIndex(m *sample.Manifest) error {
	st, err := os.Stat(m.CapturePath)
	if err != nil {
		return err
	}
	_, err = capture.LoadIndex(m.BadIndexPath, st.Size())
	if !errors.Is(err, capture.ErrIndexOutOfRange) {
		return fault.Unrecoverable("bad-index", fmt.Errorf("expected out-of-range index, got %v", err))
	}
	return nil
}

func workingBadIndex(m *sample.Manifest) (string, error) {
	b, err := os.ReadFile(m.BadIndexPath)
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.Dir, "index-working-bad.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
