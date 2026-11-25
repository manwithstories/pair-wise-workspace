package recovery

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"mediad/internal/audio"
	"mediad/internal/codec"
	"mediad/internal/fault"
	"mediad/internal/manifest"
)

// Verify independently replays transforms and checks all provenance hashes.
func Verify(root string) (*Summary, error) {
	summaryPath := filepath.Join(root, "output", "summary.json")
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		return nil, fault.New(fault.IO, summaryPath, -1, "read summary: %v", err)
	}
	var sum Summary
	if err := json.Unmarshal(data, &sum); err != nil {
		return nil, fault.New(fault.CheckpointCorrupt, summaryPath, 0, "invalid summary: %v", err)
	}

	ledger, err := LoadLedger(root)
	if err != nil {
		return nil, err
	}
	if len(ledger.Records) != sum.SegmentSHA256 || len(ledger.Records) == 0 {
		return nil, fault.New(fault.ReplayMismatch, LedgerPath(root), 0,
			"ledger records=%d want segment_sha256=%d", len(ledger.Records), sum.SegmentSHA256)
	}

	var pcmBytes []byte
	keyframeCount := 0
	adapter := codec.NewV1()
	session, err := adapter.Open(manifest.CodecDescriptor{
		Container: "fmp4", VideoCodec: "avc1", AudioCodec: "ipcm",
		AdapterVersion: codec.AdapterVersion, SpecVersion: codec.SupportedSpecVersion,
	})
	if err != nil {
		return nil, err
	}

	hlsPath, _ := inputPaths(root)
	for i, rec := range ledger.Records {
		if rec.SegmentIndex != i {
			return nil, fault.New(fault.LedgerCorrupt, LedgerPath(root), int64(i), "ledger is not ordered by segment index")
		}
		sourcePath := manifest.ResolveURI(hlsPath, rec.SourceURI)
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			return nil, fault.New(fault.IO, sourcePath, -1, "replay read source: %v", err)
		}
		if HashBytes(source) != rec.SourceSHA256 {
			return nil, fault.New(fault.ReplayMismatch, sourcePath, 0, "source hash mismatch")
		}
		fragment, err := manifest.ParseFragment(sourcePath, source)
		if err != nil {
			return nil, err
		}
		if len(fragment.Mdats) < 2 {
			return nil, fault.New(fault.MdatCorrupt, sourcePath, -1, "expected audio/video mdats")
		}
		nals, err := codec.ScanAVCC(sourcePath, fragment.Mdats[0], 0)
		if err != nil {
			return nil, err
		}
		for _, n := range nals {
			if n.IDR {
				keyframeCount++
			}
		}
		pcm, err := session.DecodePCM16LE(sourcePath, fragment.Mdats[len(fragment.Mdats)-1])
		if err != nil {
			return nil, err
		}
		out, err := processPCM(pcm, rec.Params)
		if err != nil {
			return nil, err
		}
		encoded := encodePCM16LE(out)
		if HashBytes(encoded) != rec.OutputSHA256 {
			return nil, fault.New(fault.ReplayMismatch, sourcePath, 0, "segment output hash mismatch")
		}
		pcmBytes = append(pcmBytes, encoded...)
	}

	params := ledger.Records[0].Params
	buf, err := audio.PCM16ToBuffer(decodePCM16LE(pcmBytes), params.Rate, 2)
	if err != nil {
		return nil, err
	}
	mastered, _, err := audio.Master(buf, params.LUFS, -1.0)
	if err != nil {
		return nil, err
	}
	wav, err := audio.EncodeWAV(mastered)
	if err != nil {
		return nil, err
	}
	outputPath := filepath.Join(root, "output", "audio.wav")
	storedWAV, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fault.New(fault.IO, outputPath, -1, "read output WAV: %v", err)
	}
	if HashBytes(wav) != HashBytes(storedWAV) {
		return nil, fault.New(fault.ReplayMismatch, outputPath, 0, "full replay WAV hash mismatch")
	}

	decoded, err := audio.DecodeWAV(storedWAV)
	if err != nil {
		return nil, err
	}
	sum.OutputSampleRate = decoded.SampleRate
	sum.Channels = decoded.Channels
	report := audio.Measure(decoded)
	sum.IntegratedLUFS = report.IntegratedLUFS
	sum.TruePeakDBTP = report.TruePeakDBTP
	if decoded.SampleRate != 48000 || decoded.Channels != 2 {
		return nil, fault.New(fault.ReplayMismatch, outputPath, 0, "bad output format rate=%d channels=%d",
			decoded.SampleRate, decoded.Channels)
	}
	if math.Abs(report.IntegratedLUFS-params.LUFS) > 0.5 {
		return nil, fault.New(fault.ReplayMismatch, outputPath, 0,
			"loudness %.2f differs from %.1f by >0.5 LU", report.IntegratedLUFS, params.LUFS)
	}
	if report.TruePeakDBTP > -1.0+1e-9 {
		return nil, fault.New(fault.ReplayMismatch, outputPath, 0,
			"true peak %.3f exceeds -1 dBTP", report.TruePeakDBTP)
	}
	if keyframeCount != sum.KeyframeTotal || sum.KeyframeMatched != sum.KeyframeTotal {
		return nil, fault.New(fault.ReplayMismatch, summaryPath, 0, "keyframe replay mismatch")
	}
	cp, err := LoadCheckpoint(filepath.Join(root, "work", "checkpoint.json"))
	if err != nil {
		return nil, err
	}
	for _, stage := range StageOrder {
		if !cp.StageDone(stage) {
			return nil, fault.New(fault.ReplayMismatch, cp.pathFor(root), 0, "stage %s incomplete", stage)
		}
	}

	sum.ReplayMatch = true
	sum.OK = true
	if err := WriteSummary(root, &sum); err != nil {
		return nil, err
	}
	return &sum, nil
}

// Resume loads a checkpoint and continues an interrupted run.
func Resume(root string) (*Summary, error) {
	cpPath := filepath.Join(root, "work", "checkpoint.json")
	cp, err := LoadCheckpoint(cpPath)
	if err != nil {
		return nil, err
	}
	interrupted := cp.InterruptedAt != ""
	opts := RunOptions{
		Root: root, Params: cp.Params,
		RTP: DefaultRTP(cp.Params.Damage), Resume: true,
	}
	sum, err := Run(opts)
	if err != nil {
		return nil, err
	}
	verified, err := Verify(root)
	if err != nil {
		return nil, err
	}
	sum.ReplayMatch = true
	sum.ResumeMatch = interrupted && verified.ReplayMatch && sum.ReusedSegments == sum.Segments
	if !sum.ResumeMatch {
		return nil, fault.New(fault.ReplayMismatch, cpPath, 0,
			"resume continuity check failed: interrupted=%v reused=%d segments=%d",
			interrupted, sum.ReusedSegments, sum.Segments)
	}
	sum.IntegratedLUFS = verified.IntegratedLUFS
	sum.TruePeakDBTP = verified.TruePeakDBTP
	if err := WriteSummary(root, sum); err != nil {
		return nil, err
	}
	return sum, nil
}

// WriteSummary atomically writes a summary.
func WriteSummary(root string, sum *Summary) error {
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fault.New(fault.CheckpointCorrupt, filepath.Join(root, "output", "summary.json"), -1, "encode: %v", err)
	}
	return writeFileAtomic(filepath.Join(root, "output", "summary.json"), data)
}

func (c *Checkpoint) pathFor(root string) string {
	return filepath.Join(root, "work", "checkpoint.json")
}
