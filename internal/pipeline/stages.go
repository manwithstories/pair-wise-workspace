package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"avrepair/internal/index"
	"avrepair/internal/journal"
	"avrepair/internal/manifest"
	"avrepair/internal/media"
)

func (c *runContext) stageParse() error {
	mp, err := manifest.Load(c.opt.Input)
	if err != nil {
		return err
	}
	c.man = mp
	tl, err := c.buildTimeline()
	if err != nil {
		return err
	}
	c.timeline = tl
	c.report.Manifest = mp.Source
	c.report.Tracks = len(tl.Tracks)
	c.report.Warnings = append(c.report.Warnings, tl.Warnings...)
	artifact := struct {
		Manifest *manifest.Manifest `json:"manifest"`
		Warnings []string           `json:"warnings,omitempty"`
	}{mp, tl.Warnings}
	return c.checkpoint(StageParse, artifact)
}

func (c *runContext) stageRecover(reused bool) error {
	if reused {
		return c.loadRecoverArtifact()
	}
	total, rec, unrecoverable := 0, 0, 0
	for _, track := range c.timeline.Tracks {
		for _, seg := range track.Segments {
			total += seg.MissingPackets
			rec += seg.RecoveredPacket
			unrecoverable += seg.UnrecoverGroups
		}
	}
	c.report.RecoveredPackets = rec
	c.report.Unrecovered = unrecoverable
	c.report.LossRecoveryPct = 100
	if total > 0 {
		c.report.LossRecoveryPct = float64(rec) / float64(total) * 100
	}
	artifact := struct {
		Recovered   int `json:"recovered_packets"`
		Missing     int `json:"missing_packets"`
		Unrecovered int `json:"unrecovered_groups"`
	}{rec, total, unrecoverable}
	return c.checkpoint(StageRecover, artifact)
}

func (c *runContext) loadRecoverArtifact() error {
	var a struct {
		Recovered   int `json:"recovered_packets"`
		Missing     int `json:"missing_packets"`
		Unrecovered int `json:"unrecovered_groups"`
	}
	if err := c.loadArtifact(StageRecover, &a); err != nil {
		return err
	}
	c.report.RecoveredPackets = a.Recovered
	c.report.Unrecovered = a.Unrecovered
	c.report.LossRecoveryPct = 100
	if a.Missing > 0 {
		c.report.LossRecoveryPct = float64(a.Recovered) / float64(a.Missing) * 100
	}
	if !contains(c.report.ReusedNames, StageRecover) {
		c.report.ReusedNames = append(c.report.ReusedNames, StageRecover)
	}
	return nil
}

func (c *runContext) stageRepair() error {
	hash, err := c.repairTracks()
	if err != nil {
		return err
	}
	c.hash = hash
	artifact := struct {
		Timeline *media.Timeline `json:"timeline"`
		Hash     string          `json:"output_sha256"`
		SegTimes []float64       `json:"segment_ms,omitempty"`
	}{c.timeline, hash, c.segTimes}
	return c.checkpoint(StageRepair, artifact)
}

func (c *runContext) loadRepairArtifact() error {
	var a struct {
		Timeline *media.Timeline `json:"timeline"`
		Hash     string          `json:"output_sha256"`
		SegTimes []float64       `json:"segment_ms,omitempty"`
	}
	if err := c.loadArtifact(StageRepair, &a); err != nil {
		return err
	}
	c.timeline = a.Timeline
	c.hash = a.Hash
	c.segTimes = a.SegTimes
	return c.refreshReport()
}

func (c *runContext) stageIndex() error {
	idx, err := c.buildIndex()
	if err != nil {
		return err
	}
	c.index = idx
	artifact := struct {
		Entries int    `json:"entries"`
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
	}{len(idx.Entries), c.report.IndexPath, idx.ResultHash}
	return c.checkpoint(StageIndex, artifact)
}

func (c *runContext) loadIndexArtifact() error {
	var a struct {
		Entries int    `json:"entries"`
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
	}
	if err := c.loadArtifact(StageIndex, &a); err != nil {
		return err
	}
	c.report.IndexEntries = a.Entries
	c.report.IndexPath = a.Path
	return nil
}

func (c *runContext) indexOnDiskValid() bool {
	data, err := os.ReadFile(filepath.Join(c.opt.OutputDir, "index.json"))
	if err != nil {
		return false
	}
	idx, err := index.Decode(data)
	if err != nil {
		return false
	}
	return idx.Validate(c.opt.OutputDir) == nil
}

func (c *runContext) stageFinalize() error {
	if c.timeline == nil {
		return fmt.Errorf("pipeline: finalize without timeline")
	}
	if c.hash == "" {
		hash, err := c.repairTracks()
		if err != nil {
			return err
		}
		c.hash = hash
	}
	c.report.ResultSHA256 = c.hash
	return c.checkpoint(StageFinalize, struct {
		ResultSHA256 string `json:"result_sha256"`
		IndexEntries int    `json:"seek_index_entries"`
	}{c.hash, c.report.IndexEntries})
}

func (c *runContext) checkpoint(stage string, artifact any) error {
	data, err := jsonEncode(artifact)
	if err != nil {
		return err
	}
	hash, err := c.store.WriteArtifact(stage, data)
	if err != nil {
		return err
	}
	cp, err := c.store.LoadCheckpoint()
	if err != nil {
		return err
	}
	if cp == nil || cp.InputDigest != c.digest {
		cp = &journal.Checkpoint{InputDigest: c.digest, Hashes: map[string]string{}, Artifacts: map[string]string{}}
	}
	if !contains(cp.Completed, stage) {
		cp.Completed = append(cp.Completed, stage)
	}
	cp.Hashes[stage] = hash
	cp.Artifacts[stage] = fmt.Sprintf("stage_%s.json", stage)
	if stage == StageFinalize {
		cp.Finalized = true
		cp.ResultSHA = c.hash
	}
	c.report.CompletedStages = appendUniq(c.report.CompletedStages, stage)
	return c.store.SaveCheckpoint(*cp)
}

func (c *runContext) loadArtifact(stage string, dst any) error {
	cp, err := c.store.LoadCheckpoint()
	if err != nil {
		return err
	}
	if cp == nil {
		return journal.ErrCorruptCheckpoint
	}
	hash, ok := cp.Hashes[stage]
	if !ok {
		return journal.ErrCorruptCheckpoint
	}
	data, err := c.store.ReadArtifact(stage, hash)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func (c *runContext) interrupt(stage string) (*Report, error) {
	c.report.Interrupted = true
	c.report.ReusedStages = len(c.report.ReusedNames)
	c.recordSeq++
	_ = c.store.AppendRecord(journal.Record{Seq: c.recordSeq, Stage: stage, InputDigest: c.digest, Detail: "interrupted after stage"})
	return c.report, fmt.Errorf("%w %s", ErrInterrupted, stage)
}

// refreshReport recomputes derived report fields from a restored timeline.
func (c *runContext) refreshReport() error {
	tl := c.timeline
	c.report.Tracks = len(tl.Tracks)
	c.report.Segments = countSegments(tl, nil)
	c.report.SegmentsValid = countSegments(tl, func(s *media.Segment) bool { return s.Valid() })
	c.report.Quarantined = len(tl.Quarantine)
	c.report.Corrections = len(tl.Corrections)
	c.report.AVSyncMaxMS = avSyncMax(tl).Millis()
	c.report.P95SegmentMS = percentile(c.segTimes, 0.95)
	return nil
}
