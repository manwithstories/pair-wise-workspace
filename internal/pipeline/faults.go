package pipeline

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"

	"mediapipe/internal/audio"
	"mediapipe/internal/cache"
	"mediapipe/internal/checkpoint"
	"mediapipe/internal/manifest"
	"mediapipe/internal/media"
	"mediapipe/internal/source"
	"mediapipe/internal/transport"
)

// RunFaultSuite creates local corruption/recovery fixtures and reports each
// expected rejection. Expected failures are part of a successful suite.
func RunFaultSuite(work string, emit func(manifest.FaultLine)) []manifest.FaultLine {
	dir, err := media.SafeJoin(work, "faults")
	if err != nil {
		return []manifest.FaultLine{faultFromError("workspace_path", err)}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return []manifest.FaultLine{faultFromError("io_error", media.WrapError(media.CodeIO, "create faults", err))}
	}
	var lines []manifest.FaultLine
	add := func(name string, err error, expected media.ErrorCode) {
		l := faultFromError(name, err)
		l.OK = err != nil && l.ErrorCode == string(expected)
		lines = append(lines, l)
		if emit != nil {
			emit(l)
		}
	}

	// 1. Truncated moov fixture produced from a real astiav MP4.
	valid := filepath.Join(dir, "valid_for_truncation.mp4")
	err = source.GenerateSyntheticMP4(source.SyntheticConfig{Path: valid, Seconds: 1, Width: 96, Height: 64, FPS: 8, BitrateKbps: 200})
	if err == nil {
		var b []byte
		b, err = os.ReadFile(valid)
		if err == nil && len(b) > 64 {
			err = os.WriteFile(filepath.Join(dir, "truncated_moov.mp4"), b[:len(b)-32], 0o644)
		}
		if err == nil {
			_, err = source.Probe(filepath.Join(dir, "truncated_moov.mp4"))
		}
	}
	add("truncated_moov", err, media.CodeTruncatedMoov)

	// 2. Explicit timestamp rollback fixture; no silent correction is allowed.
	rollbackPath := filepath.Join(dir, "timestamp_rollback.json")
	_ = os.WriteFile(rollbackPath, []byte(`{"stream_index":0,"position":401,"pts":[0,3003,2990]}`+"\n"), 0o644)
	rollbackErr := source.ValidateTimestamps([]media.Packet{
		{StreamIndex: 0, PTS: 0, DTS: 0, FileOffset: 100},
		{StreamIndex: 0, PTS: 3003, DTS: 3003, FileOffset: 300},
		{StreamIndex: 0, PTS: 2990, DTS: 6006, FileOffset: 401},
	})
	add("timestamp_rollback", rollbackErr, media.CodeTimestampRollback)

	// 3. Unrecoverable transport loss.
	rb := transport.NewReorderBuffer(1)
	_, lossErr := rb.Recover(4, 0, func(uint64) (media.Packet, bool, error) {
		return media.Packet{}, false, nil
	})
	add("unrecoverable_loss", lossErr, media.CodeUnrecoverableLoss)

	// 4. Tampered artifact SHA-256.
	store, err := cache.Open(filepath.Join(dir, "cache"), 8, 1<<20)
	if err != nil {
		add("digest_tampered", err, media.CodeChecksumMismatch)
	} else {
		srcFile := filepath.Join(dir, "digest_source.bin")
		_ = os.WriteFile(srcFile, []byte("digest-source"), 0o644)
		sum, _ := media.FileSHA256(srcFile)
		key, _ := cache.Key(sum, map[string]string{"case": "tampered"})
		_, _ = store.Put(key, sum, "cfg", "artifact.mp4", []byte("original-artifact"))
		artPath, _ := store.ArtifactPath(key, "artifact.mp4")
		_ = os.WriteFile(artPath, []byte("tampered"), 0o644)
		v, verifyErr := store.VerifyHit(key, srcFile)
		if verifyErr == nil {
			verifyErr = cache.StrictVerification(v, nil)
		}
		add("digest_tampered", verifyErr, media.CodeChecksumMismatch)

		// 5. Source digest changed after caching.
		srcFile2 := filepath.Join(dir, "source_digest.bin")
		_ = os.WriteFile(srcFile2, []byte("source-a"), 0o644)
		sum2, _ := media.FileSHA256(srcFile2)
		key2, _ := cache.Key(sum2, map[string]string{"case": "source"})
		_, _ = store.Put(key2, sum2, "cfg", "a.mp4", []byte("a"))
		_ = os.WriteFile(srcFile2, []byte("source-b-changed"), 0o644)
		v2, err2 := store.VerifyHit(key2, srcFile2)
		if err2 == nil {
			err2 = cache.StrictVerification(v2, nil)
		}
		add("source_digest_tampered", err2, media.CodeSourceMismatch)
	}

	// 6. Corrupt checkpoint.
	badJournal := filepath.Join(dir, "corrupt-checkpoint.json")
	_ = os.WriteFile(badJournal, []byte("{broken-json"), 0o644)
	_, checkpointErr := checkpoint.Open(badJournal, "fault", "", "")
	add("checkpoint_corrupt", checkpointErr, media.CodeCheckpoint)

	// 7. I/O error: product cannot be stat/hashed before checkpoint commit.
	j, _ := checkpoint.Open(filepath.Join(dir, "io-checkpoint.json"), "io", "", "")
	_, ioErr := j.RecordProduct("missing.mp4", filepath.Join(dir, "does-not-exist.mp4"), 1)
	add("io_error", ioErr, media.CodeIO)

	// 8. Boundary silence fixture (accepted, not a rejection).
	createBoundarySilenceFixture(dir)
	return lines
}

func createBoundarySilenceFixture(dir string) {
	const sr = 16_000
	n := 3 * sr
	samples := make([]float64, n)
	for i := sr / 2; i < n-sr; i++ {
		samples[i] = 0.3 * sineWave(float64(i)/sr, 440)
	}
	rep, err := audio.Analyze(samples, audio.Options{SampleRate: sr, WindowMS: 20, MinSilenceMS: 200})
	if err != nil {
		return
	}
	pcmPath := filepath.Join(dir, "boundary_silence.pcm")
	b := make([]byte, 2*len(samples))
	for i, v := range samples {
		x := int16(v * 32767)
		binary.LittleEndian.PutUint16(b[2*i:], uint16(x))
	}
	_ = os.WriteFile(pcmPath, b, 0o644)
	type boundaryFixture struct {
		Case           string                   `json:"case"`
		PCMPath        string                   `json:"pcm_path"`
		SampleRate     int                      `json:"sample_rate"`
		SilenceRegions []manifest.SilenceRegion `json:"silence_regions"`
		PeakDBFS       float64                  `json:"audio_peak_dbfs"`
	}
	fx := boundaryFixture{Case: "boundary_silence", PCMPath: pcmPath, SampleRate: sr, PeakDBFS: rep.PeakDBFS}
	for _, r := range rep.Regions {
		fx.SilenceRegions = append(fx.SilenceRegions, manifest.SilenceRegion{
			StartSec: r.StartSec, EndSec: r.EndSec, DurationSec: r.DurationSec, Boundary: r.Boundary,
		})
	}
	path := filepath.Join(dir, "boundary_silence.json")
	_, _, _ = manifest.WriteAtomicJSON(path, fx)
}

func faultFromError(name string, err error) manifest.FaultLine {
	l := manifest.FaultLine{Event: "fault", Case: name}
	if err == nil {
		l.ErrorCode = "not_rejected"
		l.ExitCode = 1
		l.OK = false
		l.Detail = "expected error but operation succeeded"
		return l
	}
	if me, ok := media.AsError(err); ok {
		l.ErrorCode = string(me.Code)
		l.ExitCode = me.ExitCode()
		l.Detail = me.Error()
		return l
	}
	l.ErrorCode = "unknown"
	l.ExitCode = 1
	l.Detail = err.Error()
	return l
}

func sineWave(t float64, hz float64) float64 {
	return math.Sin(2 * math.Pi * hz * t)
}
