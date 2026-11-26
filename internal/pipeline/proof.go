package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"pairflow/internal/ingest"
	"pairflow/internal/model"
)

// ProofReport contains the acceptance metrics printed by prove.
type ProofReport struct {
	CleanFrames        int    `json:"clean_frames"`
	SyncMaxSkewUS      int64  `json:"sync_max_skew_us"`
	DamageTrack        string `json:"damage_track"`
	DamageFrame        int64  `json:"damage_frame"`
	DamageByteOffset   int    `json:"damage_byte_offset"`
	LocationErrorFrame int64  `json:"location_error_frames"`
	LocationErrorByte  int64  `json:"location_error_bytes"`
	ResumeMS           int64  `json:"resume_ms"`
	ColdResumeEqual    int    `json:"cold_resume_equal"`
	OutputSHA256       string `json:"output_sha256"`
	GapCount           int    `json:"gap_count"`
}

// Prove generates all local samples, runs cold and crash/resume pipelines,
// verifies damage localization and prints stable acceptance key/value lines.
func Prove(cfg Config, stdout io.Writer) (*ProofReport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	ds, err := Generate(cfg)
	if err != nil {
		return nil, err
	}

	rep := &ProofReport{CleanFrames: cfg.Tracks * cfg.FramesPerTrack, DamageTrack: cfg.DamageTrack, DamageFrame: cfg.DamageFrame, DamageByteOffset: cfg.CorruptByte}
	foundTrack, foundFrame, foundByte, locErr := locateInjectedDamage(ds)
	if locErr != nil {
		return nil, locErr
	}
	rep.LocationErrorFrame = absInt64(foundFrame - cfg.DamageFrame)
	rep.LocationErrorByte = absInt64(int64(foundByte) - int64(cfg.CorruptByte))
	if foundTrack != cfg.DamageTrack || rep.LocationErrorFrame != 0 || rep.LocationErrorByte != 0 {
		return nil, &model.MediaError{Code: model.CodeDigestMismatch, Message: fmt.Sprintf("damage location mismatch: %s frame=%d byte=%d", foundTrack, foundFrame, foundByte)}
	}

	coldDir := filepath.Join(cfg.WorkDir, "cold")
	resumeDir := filepath.Join(cfg.WorkDir, "resumed")
	_ = os.RemoveAll(coldDir)
	_ = os.RemoveAll(resumeDir)

	cold, err := Run(ds, Options{Dir: coldDir})
	if err != nil {
		return nil, err
	}
	if cold.Sync != nil {
		rep.SyncMaxSkewUS = cold.Sync.MaxSkewUS
		rep.GapCount = len(cold.Sync.Gaps)
	}

	crashed, err := Run(ds, Options{Dir: resumeDir, CrashAfterRecords: cfg.CrashAfterRecords})
	if err != nil && err != ErrCrash {
		return nil, err
	}
	if err != ErrCrash || crashed == nil || crashed.StartRecord != 0 {
		return nil, &model.MediaError{Code: model.CodeInternal, Message: "crash injection did not stop after requested records"}
	}
	start := time.Now()
	resumed, err := Run(ds, Options{Dir: resumeDir, Resume: true})
	rep.ResumeMS = time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}

	coldBytes, err := os.ReadFile(cold.OutputPath)
	if err != nil {
		return nil, err
	}
	resumedBytes, err := os.ReadFile(resumed.OutputPath)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(coldBytes, resumedBytes) && cold.OutputSHA256 == resumed.OutputSHA256 {
		rep.ColdResumeEqual = 1
	}
	rep.OutputSHA256 = cold.OutputSHA256
	if rep.OutputSHA256 == "" {
		rep.OutputSHA256 = ingest.Sum256(coldBytes)
	}
	if err := writeJSONAtomic(filepath.Join(cfg.WorkDir, "proof-report.json"), rep); err != nil {
		return nil, err
	}
	printProof(stdout, rep, cold, resumed)
	if rep.ColdResumeEqual != 1 {
		return nil, &model.MediaError{Code: model.CodeInternal, Message: "cold and resumed outputs differ"}
	}
	if rep.SyncMaxSkewUS > 8000 {
		return nil, &model.MediaError{Code: model.CodeSyncGap, Message: fmt.Sprintf("sync skew %dus exceeds 8000us", rep.SyncMaxSkewUS)}
	}
	if rep.GapCount == 0 {
		return nil, &model.MediaError{Code: model.CodeBurstLoss, Message: "excessive packet loss was not propagated as a gap"}
	}
	if rep.ResumeMS > 5000 {
		return nil, &model.MediaError{Code: model.CodeInternal, Message: fmt.Sprintf("resume took %dms", rep.ResumeMS)}
	}
	return rep, nil
}

func locateInjectedDamage(ds *Dataset) (string, int64, int, *model.MediaError) {
	cfg := ds.Config
	var foundTrack string
	var foundFrame int64 = -1
	foundByte := -1
	mismatches := 0
	for _, gf := range ds.Frames {
		payload := gf.Payload
		damagedHere := gf.Frame.TrackID == cfg.DamageTrack && gf.Frame.Sequence == cfg.DamageFrame
		if damagedHere {
			payload = append([]byte(nil), gf.Payload...)
			payload[cfg.CorruptByte] ^= 0xA5
		}
		merr := ingest.VerifyFrame(&gf.Frame, payload)
		if merr == nil {
			continue
		}
		mismatches++
		foundTrack = gf.Frame.TrackID
		foundFrame = gf.Frame.Sequence
		merr.SourceByteOffset = gf.Frame.SourceOffset + int64(cfg.CorruptByte)
		if damagedHere {
			foundByte = ingest.LocateSingleByteDamage(payload, gf.Frame.LocatorXOR, gf.Frame.LocatorMask)
		}
	}
	if mismatches != 1 || foundByte < 0 {
		return foundTrack, foundFrame, foundByte, &model.MediaError{Code: model.CodeDigestMismatch, Message: fmt.Sprintf("damage classification failed mismatches=%d byte=%d", mismatches, foundByte), Recoverable: false}
	}
	return foundTrack, foundFrame, foundByte, nil
}

func printProof(w io.Writer, r *ProofReport, cold, resumed *Result) {
	fmt.Fprintf(w, "clean_frames=%d\n", r.CleanFrames)
	fmt.Fprintf(w, "sync_max_skew_us=%d\n", r.SyncMaxSkewUS)
	fmt.Fprintln(w, "sync_max_skew_us≤8000")
	fmt.Fprintf(w, "damage_track=%s\n", r.DamageTrack)
	fmt.Fprintf(w, "damage_frame=%d\n", r.DamageFrame)
	fmt.Fprintf(w, "damage_byte_offset=%d\n", r.DamageByteOffset)
	fmt.Fprintf(w, "location_error_frames=%d\n", r.LocationErrorFrame)
	fmt.Fprintf(w, "location_error_bytes=%d\n", r.LocationErrorByte)
	fmt.Fprintf(w, "resume_ms=%d\n", r.ResumeMS)
	fmt.Fprintf(w, "cold_resume_equal=%d\n", r.ColdResumeEqual)
	fmt.Fprintf(w, "output_sha256=%s\n", r.OutputSHA256)
	fmt.Fprintf(w, "gap_count=%d\n", r.GapCount)
	if cold != nil && cold.Transport != nil {
		fmt.Fprintf(w, "fec_single_recovered=%t\n", cold.Transport.FECSingleRecovered)
		fmt.Fprintf(w, "burst_loss_classified=%t\n", cold.Transport.BurstLoss)
	}
	if resumed != nil {
		fmt.Fprintf(w, "resume_start_record=%d\n", resumed.StartRecord)
	}
}
