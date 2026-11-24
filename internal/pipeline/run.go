package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/clearliu/mediq/internal/core"
	"github.com/clearliu/mediq/internal/manifest"
	"github.com/clearliu/mediq/internal/pack"
	"github.com/clearliu/mediq/internal/recovery"
	"github.com/clearliu/mediq/internal/source"
)

const (
	InputName    = "input.lenrtp"
	ProductName  = "mediq-delivery.mp4"
	ManifestName = "manifest.json"
	chunkFrames  = int64(24_000)
)

// Options configures one demo invocation.
type Options struct {
	Case       string
	OutDir     string
	Resume     bool
	Strict     bool
	CrashAfter string
}

// ValidCase reports whether name is a supported demo case.
func ValidCase(name string) bool {
	return name == "clean" || name == "damaged" || name == "recovery"
}

// Run executes the pipeline and returns the manifest when enough state exists.
// A non-nil manifest may accompany an interrupt error so the CLI can print
// quality counters before exiting 75.
func Run(opts Options) (*manifest.Manifest, error) {
	if opts.OutDir == "" {
		return nil, core.New(core.KindConfig, "options", fmt.Errorf("--out is required"))
	}
	if !ValidCase(opts.Case) {
		return nil, core.New(core.KindConfig, "options", fmt.Errorf("unknown case %q", opts.Case))
	}
	if opts.CrashAfter != "" && opts.CrashAfter != "checkpoint" {
		return nil, core.New(core.KindConfig, "options", fmt.Errorf("unsupported crash point %q", opts.CrashAfter))
	}
	spec := source.CaseSpec(opts.Case)
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, core.New(core.KindIO, "create-out", err)
	}

	inputPath := filepath.Join(opts.OutDir, InputName)
	inputBytes, inputSHA, err := ensureInput(inputPath, spec)
	if err != nil {
		return nil, err
	}
	params := makeParams(spec)
	paramsSHA, err := recovery.DigestJSON(params)
	if err != nil {
		return nil, err
	}

	store := recovery.NewStore(opts.OutDir)
	cp, err := store.Load()
	if err != nil {
		return nil, err
	}
	if opts.Resume {
		if cp == nil {
			return nil, core.New(core.KindConfig, "resume", fmt.Errorf("no checkpoint at %s", store.Path))
		}
		if err := cp.ValidateResume(inputSHA, paramsSHA); err != nil {
			return nil, err
		}
	} else {
		cp = nil
	}

	a, err := Analyze(inputBytes, spec)
	if err != nil {
		return nil, err
	}
	missing := a.MissingPackets()
	missingSamples := int64(len(missing)) * OutputFramesPerPacket(spec)
	if opts.Strict && (a.ParseErrors > 0 || missingSamples > 0 || a.Watermark.LateExpired > 0) {
		return nil, core.New(core.KindStrict, "strict",
			fmt.Errorf("input has parse_errors=%d missing_samples=%d late_expired=%d",
				a.ParseErrors, missingSamples, a.Watermark.LateExpired))
	}

	src, err := BuildSourceBuffer(a, spec)
	if err != nil {
		return nil, core.New(core.KindParse, "decode-source", err)
	}
	pcm, norm, _, err := ConvertAudio(src)
	if err != nil {
		return nil, core.New(core.KindParse, "convert-audio", err)
	}
	totalFrames := int64(len(pcm) / (core.OutputChannels * core.OutputBytesPerSample))
	durationMS := float64(totalFrames) * 1000 / core.OutputRateHz
	expectedMS := float64(spec.PacketCount*spec.FramesPerPacket) * 1000 / float64(spec.SampleRate)
	actualBPS := float64(len(pcm)*8) / (durationMS / 1000)
	deviationPct := math.Abs(actualBPS-float64(core.TargetBPS)) / float64(core.TargetBPS) * 100

	m := &manifest.Manifest{
		Status:                "completed",
		ExitCode:              0,
		Strict:                opts.Strict,
		FirstParseErrorOffset: a.FirstOffset,
		FirstParseError:       a.FirstError,
		InputExceptionCount:   a.ParseErrors,
		Source: manifest.Source{
			Case: opts.Case, File: InputName, SHA256: inputSHA, SSRC: spec.SSRC,
			PayloadType: spec.PayloadType, SampleRateHz: spec.SampleRate, Channels: spec.Channels,
			FramesPerPacket: spec.FramesPerPacket, RTPSeqFirst: 0, RTPSeqLast: uint16(spec.PacketCount - 1),
			ExtendedSeqFirst: a.ExtFirst, ExtendedSeqLast: a.ExtLast,
			RTPTimestampFirst: 0, RTPTimestampLast: uint32((spec.PacketCount - 1) * spec.FramesPerPacket),
			StartTimeMS: 0, EndTimeMS: expectedMS,
		},
		Params: params,
		Counts: manifest.Counts{
			RTPFrames: a.Reorder.Stats.RTPFrames, RTCPFrames: a.RTCPFrames,
			ParseErrors: a.ParseErrors, DuplicatePackets: a.DuplicatePackets,
			OutOfOrder: a.OutOfOrder, GapPackets: a.Reorder.Stats.GapPackets,
			LatePackets: a.Reorder.Stats.LatePackets, LateReplays: a.Watermark.LateReplays,
			LateExpired: a.Watermark.LateExpired, MissingSamples: missingSamples,
			MissingSeqRanges: FormatSeqRanges(missing),
		},
		Quality: manifest.Quality{
			TargetBPS: core.TargetBPS, ActualBPS: actualBPS, DeviationPct: deviationPct,
			DurationMS: durationMS, ExpectedMS: expectedMS,
			ManifestErrorMS: math.Abs(durationMS - expectedMS),
			IntegratedLUFS:  norm.OutputLUFS, TruePeakDBTP: norm.TruePeakDBTP,
		},
	}

	stage := &pack.PCMStage{Path: filepath.Join(opts.OutDir, "audio.pcm"), Channels: core.OutputChannels}
	startFrames := int64(0)
	if cp != nil {
		startFrames = cp.CommittedOutputSamples
	}
	for pos := startFrames; pos < totalFrames; {
		end := pos + chunkFrames
		if end > totalFrames {
			end = totalFrames
		}
		byteStart := pos * int64(core.OutputChannels*core.OutputBytesPerSample)
		byteEnd := end * int64(core.OutputChannels*core.OutputBytesPerSample)
		if err := stage.AppendAt(pos, pcm[byteStart:byteEnd]); err != nil {
			return m, err
		}
		newCP := recovery.Checkpoint{
			Case: opts.Case, InputSHA256: inputSHA, ParamsSHA256: paramsSHA,
			CommittedOutputSamples: end, CommittedExtendedSeq: a.ExtLast,
			ProcessedRecords: a.Reorder.Stats.AcceptedPackets + a.RTCPFrames + a.ParseErrors,
			Completed:        end == totalFrames,
		}
		if err := store.Save(newCP); err != nil {
			return m, err
		}
		if opts.CrashAfter == "checkpoint" {
			m.CommittedOutputSamples = end
			return m, core.New(core.KindInterrupted, "injected", fmt.Errorf("crash after checkpoint at output_samples=%d", end))
		}
		pos = end
	}

	staged, err := stage.ReadAll()
	if err != nil {
		return m, err
	}
	if string(staged) != string(pcm) {
		return m, core.New(core.KindCheckpoint, "verify-pcm-stage", fmt.Errorf("staged PCM does not match deterministic output"))
	}
	productPath := filepath.Join(opts.OutDir, ProductName)
	prod, err := pack.WriteLPCM(productPath, pcm, pack.Options{SampleRate: core.OutputRateHz, Channels: core.OutputChannels})
	if err != nil {
		return m, err
	}
	m.Product = manifest.Product{File: ProductName, SHA256: prod.SHA256, Bytes: prod.Bytes, PCMBytes: prod.PCMBytes}
	m.CommittedOutputSamples = totalFrames

	finalCP := recovery.Checkpoint{
		Case: opts.Case, InputSHA256: inputSHA, ParamsSHA256: paramsSHA,
		CommittedOutputSamples: totalFrames, CommittedExtendedSeq: a.ExtLast,
		ProcessedRecords: a.Reorder.Stats.AcceptedPackets + a.RTCPFrames + a.ParseErrors,
		Completed:        true, ProductSHA256: prod.SHA256,
	}
	if err := store.Save(finalCP); err != nil {
		return m, err
	}
	if sum, err := recovery.SHA256File(store.Path); err == nil {
		m.CheckpointSHA256 = sum
	}
	manifestPath := filepath.Join(opts.OutDir, ManifestName)
	if err := manifest.Write(manifestPath, m); err != nil {
		return m, err
	}
	return m, nil
}

func ensureInput(path string, spec source.Spec) ([]byte, string, error) {
	generated, err := source.Generate(spec)
	if err != nil {
		return nil, "", core.New(core.KindConfig, "generate-fixture", err)
	}
	// The fixture is local and deterministic; writing it on every invocation
	// makes a interrupted first run and a resumed run observe identical bytes.
	if err := os.WriteFile(path, generated, 0o644); err != nil {
		return nil, "", core.New(core.KindIO, "write-fixture", err)
	}
	sum := sha256.Sum256(generated)
	return generated, hex.EncodeToString(sum[:]), nil
}

func makeParams(spec source.Spec) manifest.Params {
	resampler := "48k-passthrough"
	if spec.SampleRate == 44100 {
		resampler = "windowed-sinc-rational-160/147"
	}
	return manifest.Params{
		OutputRateHz: core.OutputRateHz, OutputChannels: core.OutputChannels,
		OutputBitsPerSample: 16, TargetBPS: core.TargetBPS, Resampler: resampler,
		LoudnessTargetLUFS: dspTarget(), TruePeakCeilingDBTP: -1,
		ReorderWindow: spec.ReorderWindow, TimelineIntervalMS: spec.IntervalSamples * 1000 / core.OutputRateHz,
		RetentionIntervals: spec.RetentionIntervals, MaxDatagramBytes: 8192,
	}
}

func dspTarget() float64 { return -23 }
