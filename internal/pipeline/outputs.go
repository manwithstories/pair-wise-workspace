package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"avrepair/internal/journal"
	"avrepair/internal/media"
	"avrepair/internal/recovery"
	"avrepair/internal/segment"
)

func (c *runContext) collectQuarantine(tl *media.Timeline) {
	tl.Quarantine = tl.Quarantine[:0]
	for _, track := range tl.Tracks {
		for _, seg := range track.Segments {
			if seg.Quarantine == "" {
				continue
			}
			tl.Quarantine = append(tl.Quarantine, media.Quarantine{
				Segment: seg.ID, Track: track.ID, URI: seg.URI,
				Reason: seg.Quarantine, Detail: seg.QuarantineCause,
			})
			c.recordSeq++
			_ = c.store.AppendRecord(journal.Record{
				Seq: c.recordSeq, Stage: StageRepair, InputDigest: c.digest, Track: track.ID,
				Segment: seg.ID, Source: seg.URI, SourceOffset: fmt.Sprintf("byte=%d+%d", seg.ByteOffset, seg.ByteLength),
				Detail: "quarantined: " + seg.Quarantine + ": " + seg.QuarantineCause,
			})
		}
	}
}

// writeOutputs materializes repaired segment payloads in the output dir.
func (c *runContext) writeOutputs(tl *media.Timeline) error {
	if err := os.MkdirAll(c.opt.OutputDir, 0o755); err != nil {
		return err
	}
	c.segTimes = c.segTimes[:0]
	for _, track := range tl.Tracks {
		for _, seg := range track.Segments {
			if !seg.Valid() {
				continue
			}
			start := time.Now()
			data, err := c.outputBytes(seg)
			if err != nil {
				seg.QuarantineWith(media.ReasonUnparseable, err.Error())
				continue
			}
			name := fmt.Sprintf("%s_%05d.bin", sanitize(track.ID), seg.Seq)
			path := filepath.Join(c.opt.OutputDir, name)
			elapsed := float64(time.Since(start).Microseconds()) / 1000.0
			if err := atomicWrite(path, data); err != nil {
				return err
			}
			seg.Output = name
			seg.OutputSHA = sha256Hex(data)
			// The metric measures in-memory repair work per segment and
			// excludes the atomic write so it is not disk-latency bound.
			c.segTimes = append(c.segTimes, elapsed)
			if err := c.writeSegmentRecord(track, seg); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *runContext) outputBytes(seg *media.Segment) ([]byte, error) {
	cap := c.captures[seg.ID]
	if cap == nil || cap.err != nil {
		return nil, fmt.Errorf("pipeline: missing capture for %s", seg.ID)
	}
	if cap.format != "ts-capture" {
		return cap.data, nil
	}
	pkts, err := segment.DecodePackets(cap.data)
	if err != nil {
		return nil, err
	}
	res := recovery.Recover(pkts)
	if len(res.Unrecoverable) > 0 {
		return nil, fmt.Errorf("pipeline: unrecoverable packets in %s", seg.ID)
	}
	return segment.EncodePackets(res.Packets), nil
}

func (c *runContext) writeSegmentRecord(track *media.Track, seg *media.Segment) error {
	scale := seg.Timescale
	if scale == 0 {
		scale = 90000
	}
	before := media.Ticks(seg.RawStart, scale).String()
	after := ""
	if len(seg.Samples) > 0 {
		after = seg.Samples[0].PTS.String()
	}
	// SourceOffset records where the bytes came from: the manifest byte
	// range when present, otherwise the original unwrapped clock value.
	offset := fmt.Sprintf("raw_start=%s byte=%d+%d", before, seg.ByteOffset, seg.ByteLength)
	c.recordSeq++
	return c.store.AppendRecord(journal.Record{
		Seq: c.recordSeq, Stage: StageRepair, InputDigest: c.digest, Track: track.ID, Segment: seg.ID,
		Source: seg.Source, SourceOffset: offset,
		BeforePTS: before, AfterPTS: after, OutputSHA256: seg.OutputSHA,
	})
}

// buildIndex indexes only valid sync samples and commits atomically.
