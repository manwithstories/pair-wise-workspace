package pipeline

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"pairflow/internal/clock"
	"pairflow/internal/ingest"
	"pairflow/internal/model"
	"pairflow/internal/store"
	"pairflow/internal/syncer"
)

// ErrCrash is returned after the requested records and checkpoint are durable.
// It represents a process kill, not a pipeline correctness failure.
var ErrCrash = errors.New("simulated process crash after durable checkpoint")

// Options configures one pipeline invocation.
type Options struct {
	Dir               string
	Resume            bool
	CrashAfterRecords int
	CheckpointEvery   int
}

// Result is a completed or interrupted pipeline invocation.
type Result struct {
	OutputPath     string           `json:"output_path"`
	ResultPath     string           `json:"result_path"`
	TotalRecords   int              `json:"total_records"`
	StartRecord    int              `json:"start_record"`
	OutputSHA256   string           `json:"output_sha256"`
	InputDigest    string           `json:"input_digest"`
	NormalizedMeta string           `json:"normalized_metadata_digest"`
	CacheKey       string           `json:"cache_key"`
	Sync           *syncer.Report   `json:"sync"`
	Transport      *TransportReport `json:"transport"`
}

// Run executes validation, clock mapping, transport recovery, sync, output and
// checkpointing. Resume loads and verifies the nearest valid checkpoint.
func Run(ds *Dataset, opts Options) (*Result, error) {
	if ds == nil {
		return nil, &model.MediaError{Code: model.CodeInternal, Message: "nil dataset"}
	}
	if err := ds.Config.Validate(); err != nil {
		return nil, err
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = 120
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}

	manifest := &ingest.Manifest{
		AlgorithmVersion: model.AlgorithmVersion,
		Source:           ingest.Source{Fingerprint: ds.SourceFingerprint, Kind: "generated-local"},
		Tracks:           ds.Tracks,
		Audio:            &ds.Audio,
	}
	for _, gf := range ds.Frames {
		if merr := ingest.VerifyFrame(&gf.Frame, gf.Payload); merr != nil {
			return nil, merr
		}
		manifest.Frames = append(manifest.Frames, ingest.FrameData{Meta: gf.Frame})
	}
	if err := manifest.ValidateMetadata(); err != nil {
		return nil, err
	}

	points := mapTimeline(ds)
	transportReport, err := RunTransportSample(ds.Config.DropSeqs)
	if err != nil {
		return nil, err
	}
	syncReport := syncer.Align(points, syncer.AudioGrid{SampleRateHz: model.AudioSampleRateHz, FirstPTSNS: 0}, transportReport.Gaps)

	inputDigest, metaDigest := datasetDigests(ds)
	outPath := filepath.Join(opts.Dir, "output.jsonl")
	start, outFile, err := openOutputForRun(opts.Dir, outPath, opts.Resume, inputDigest)
	if err != nil {
		return nil, err
	}
	defer outFile.Close()

	writer := bufio.NewWriter(outFile)
	result := &Result{OutputPath: outPath, TotalRecords: len(ds.Frames), StartRecord: start, Sync: syncReport, Transport: transportReport, InputDigest: inputDigest, NormalizedMeta: metaDigest}

	cas, err := store.NewCAS(filepath.Join(opts.Dir, "cas"))
	if err != nil {
		return nil, err
	}

	crashed := false
	for i := start; i < len(ds.Frames); i++ {
		gf := ds.Frames[i]
		rec := model.FrameRecord{
			Record:       "frame",
			TrackID:      gf.Frame.TrackID,
			TrackIndex:   gf.Frame.TrackIndex,
			Sequence:     gf.Frame.Sequence,
			PTSNS:        gf.Frame.PTSNS,
			DurationNS:   gf.Frame.DurationNS,
			SourceOffset: gf.Frame.SourceOffset,
			SHA256:       gf.Frame.SHA256,
			Status:       "clean",
			ExtendedTS:   gf.Frame.ExtendedTimestamp,
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			return nil, err
		}
		count := i + 1
		periodic := count%opts.CheckpointEvery == 0
		crashHere := opts.CrashAfterRecords > 0 && count == opts.CrashAfterRecords
		if periodic || crashHere {
			if err := writer.Flush(); err != nil {
				return nil, err
			}
			if err := outFile.Sync(); err != nil {
				return nil, err
			}
			if err := saveRecordCheckpoint(opts.Dir, outPath, count, inputDigest); err != nil {
				return nil, err
			}
		}
		if crashHere {
			crashed = true
			break
		}
	}
	if crashed {
		return result, ErrCrash
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	if err := outFile.Sync(); err != nil {
		return nil, err
	}
	outputBytes, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	result.OutputSHA256 = ingest.Sum256(outputBytes)
	result.ResultPath = filepath.Join(opts.Dir, "result.json")
	if err := writeJSONAtomic(result.ResultPath, result); err != nil {
		return nil, err
	}
	if _, err := store.SaveCheckpoint(opts.Dir, store.Checkpoint{Stage: "result", CompletedRecords: len(ds.Frames), OutputPath: outPath, OutputBytes: int64(len(outputBytes)), OutputSHA256: result.OutputSHA256, InputSHA256: inputDigest}); err != nil {
		return nil, err
	}
	keyInputs := store.CacheKeyInputs{SourceFingerprint: ds.SourceFingerprint, InputDigest: inputDigest, NormalizedMeta: metaDigest, Stage: "result"}
	key := store.CacheKey(keyInputs)
	blobDigest, err := cas.PutBlob(outputBytes)
	if err != nil {
		return nil, err
	}
	if err := cas.Put(store.Entry{Key: key, Inputs: keyInputs, SourceFingerprint: ds.SourceFingerprint, InputDigest: inputDigest, OutputDigest: blobDigest, OutputPath: outPath}); err != nil {
		return nil, err
	}
	if _, blob, merr := cas.GetRevalidated(key, ds.SourceFingerprint, inputDigest); merr != nil {
		return nil, merr
	} else if ingest.Sum256(blob) != result.OutputSHA256 {
		return nil, &model.MediaError{Code: model.CodeCacheMismatch, Message: "cache round-trip digest mismatch"}
	}
	result.CacheKey = key
	return result, nil
}

func mapTimeline(ds *Dataset) []syncer.FramePoint {
	byTrack := map[string][]*GeneratedFrame{}
	for _, gf := range ds.Frames {
		byTrack[gf.Frame.TrackID] = append(byTrack[gf.Frame.TrackID], gf)
	}
	var points []syncer.FramePoint
	for _, track := range ds.Tracks {
		frames := byTrack[track.ID]
		u := clock.NewUnwrapper()
		samples := make([]clock.Sample, 0, len(frames))
		for _, gf := range frames {
			ext, _ := u.Observe(gf.Frame.RTPTimestamp)
			gf.Frame.ExtendedTimestamp = ext
			samples = append(samples, clock.Sample{ExtendedTS: ext, ReferenceNS: gf.ReferenceNS})
		}
		drift, _ := clock.EstimateDrift(track.ClockRateHz, samples)
		for _, gf := range frames {
			gf.Frame.PTSNS = drift.Map(gf.Frame.ExtendedTimestamp)
			points = append(points, syncer.FramePoint{TrackID: track.ID, Sequence: gf.Frame.Sequence, PTSNS: gf.Frame.PTSNS, DurationNS: gf.Frame.DurationNS})
		}
	}
	return points
}

func datasetDigests(ds *Dataset) (string, string) {
	h := newSHA()
	for _, gf := range ds.Frames {
		_, _ = h.Write([]byte(gf.Frame.SHA256))
	}
	_, _ = h.Write([]byte(ds.Audio.SHA256))
	inputDigest := sumHex(h)
	meta, _ := json.Marshal(ds.Tracks)
	return inputDigest, ingest.Sum256(meta)
}

func openOutputForRun(dir, outPath string, resume bool, inputDigest string) (int, *os.File, error) {
	if !resume {
		f, err := os.Create(outPath)
		return 0, f, err
	}
	stages := []string{"manifest", "transport", "sync", "records", "result"}
	cp, _ := store.RecoverCheckpoint(dir, stages)
	if cp == nil || cp.CompletedRecords <= 0 {
		f, err := os.Create(outPath)
		return 0, f, err
	}
	f, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, nil, err
	}
	if cp.OutputBytes > 0 {
		if err := f.Truncate(cp.OutputBytes); err != nil {
			f.Close()
			return 0, nil, err
		}
		if _, err := f.Seek(cp.OutputBytes, 0); err != nil {
			f.Close()
			return 0, nil, err
		}
	} else {
		if _, err := f.Seek(0, os.SEEK_END); err != nil {
			f.Close()
			return 0, nil, err
		}
	}
	return cp.CompletedRecords, f, nil
}

func saveRecordCheckpoint(dir, outPath string, count int, inputDigest string) error {
	b, err := os.ReadFile(outPath)
	if err != nil {
		return err
	}
	_, err = store.SaveCheckpoint(dir, store.Checkpoint{Stage: "records", CompletedRecords: count, OutputPath: outPath, OutputBytes: int64(len(b)), OutputSHA256: ingest.Sum256(b), InputSHA256: inputDigest})
	return err
}
