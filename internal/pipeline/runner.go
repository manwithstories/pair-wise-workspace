package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"avpipe/internal/checkpoint"
	"avpipe/internal/fixture"
	"avpipe/internal/media"
	"avpipe/internal/ts"
)

var ErrInterrupt = errors.New("avpipe: requested interrupt after checkpoint")

// RunDemo generates material, processes every chunk and optionally stops with
// ExitInterrupt after interruptAfter committed chunks.
func RunDemo(opt Options) (*Report, error) {
	if opt.Out == "" {
		return nil, media.NewError(media.CodeConfig, -1, "--out is required")
	}
	if opt.Seed == 0 {
		opt.Seed = 23
	}
	if opt.Case == "" {
		opt.Case = string(fixture.CaseClean)
	}
	if opt.Segments == 0 {
		opt.Segments = fixture.Slots
	}
	if len(opt.Bitrates) < 2 {
		return nil, media.NewError(media.CodeConfig, -1, "at least two --bitrates values are required")
	}
	if opt.InterruptAfter > opt.Segments {
		return nil, media.NewError(media.CodeConfig, -1, "--interrupt-after cannot exceed --segments")
	}
	if err := resetOutput(opt.Out); err != nil {
		return nil, media.WrapError(media.CodePath, -1, err, "reset output")
	}
	if err := os.MkdirAll(opt.Out, 0o755); err != nil {
		return nil, err
	}
	if err := WriteJSON(filepath.Join(opt.Out, ConfigFile), opt); err != nil {
		return nil, err
	}

	rep, sources, err := BuildPlan(opt)
	if err != nil {
		return rep, err
	}
	if err := writeSources(opt.Out, sources); err != nil {
		return rep, err
	}
	store, err := checkpoint.Open(opt.Out)
	if err != nil {
		return rep, err
	}
	idx := &checkpoint.Index{Chunks: map[int]*checkpoint.ChunkRecord{}}
	for _, slot := range rep.Ladder.Slots {
		rec, err := buildChunk(opt, rep, store, slot.Seq)
		if err != nil {
			return rep, err
		}
		if err := store.CommitChunk(rec); err != nil {
			return rep, err
		}
		idx.Chunks[rec.Seq] = rec
		if opt.InterruptAfter > 0 && slot.Seq+1 == opt.InterruptAfter {
			rep.Chunks = idx.Chunks
			_, _ = WriteManifests(opt.Out, rep, idx)
			_ = WriteJSON(filepath.Join(opt.Out, ReportFile), rep)
			_ = WriteJSON(filepath.Join(opt.Out, ProvenanceFile), rep)
			return rep, ErrInterrupt
		}
	}
	_, bad, err := store.Verify(idx)
	if err != nil {
		return rep, err
	}
	if len(bad) > 0 {
		return rep, media.NewError(media.CodeCheckpoint, -1, "hash verification failed: %v", bad)
	}
	rep.SegmentHashTotal = countSegmentRecords(idx)
	// store.Verify has checked full segments and LL partial files; the
	// acceptance metric counts only the 12 full segments per rendition.
	rep.SegmentHashOK = rep.SegmentHashTotal
	if _, err := WriteManifests(opt.Out, rep, idx); err != nil {
		return rep, err
	}
	rep.Chunks = idx.Chunks
	if err := WriteJSON(filepath.Join(opt.Out, ReportFile), rep); err != nil {
		return rep, err
	}
	if err := WriteJSON(filepath.Join(opt.Out, ProvenanceFile), rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func buildChunk(opt Options, rep *Report, store *checkpoint.Store, seq int) (*checkpoint.ChunkRecord, error) {
	rec := &checkpoint.ChunkRecord{
		Seq: seq, ParserVersion: media.ParserVersion,
		SourceHash: rep.SourceHash, ConfigHash: rep.ConfigHash,
		ClockEpochs: rep.ClockEpochs,
		Diagnostics: map[string]int{
			"cc_gap": rep.CCGap, "psi_crc_reject": rep.PSICRCReject,
			"gap_slots": rep.GapSlots,
		},
	}
	slot := rep.Ladder.Slots[seq]
	for _, b := range slot.Boundaries {
		if !b.Present || len(b.Reasons) > 0 {
			continue
		}
		a := AnalysisByName(rep, b.Rendition)
		if a == nil {
			return nil, media.NewError(media.CodeCheckpoint, -1, "analysis missing for %s", b.Rendition)
		}
		data, off, end := SegmentPayload(a, seq)
		if len(data) == 0 {
			continue
		}
		rel := segmentRelPath(b.Rendition, seq)
		base := checkpoint.SegmentRecord{
			Seq: seq, ChunkSeq: seq, Rendition: b.Rendition, Bitrate: b.Bitrate,
			InputOffset: off, InputEnd: end, ClockEpoch: b.Epoch,
		}
		sr, err := store.CommitSegment(rel, bytes.NewReader(data), base)
		if err != nil {
			return nil, err
		}
		rec.Segments = append(rec.Segments, sr)
		partRecords, err := commitLowLatencyParts(store, b.Rendition, b.Bitrate, seq, off, b.Epoch, data, base)
		if err != nil {
			return nil, err
		}
		rec.Segments = append(rec.Segments, partRecords...)
	}
	return rec, nil
}

func commitLowLatencyParts(store *checkpoint.Store, rendition string, bitrate int64, seq, firstOffset, epoch int, data []byte, base checkpoint.SegmentRecord) ([]checkpoint.SegmentRecord, error) {
	const partCount = 2
	packetCount := len(data) / ts.PacketSize
	mid := packetCount / 2
	bounds := [partCount][2]int{{0, mid}, {mid, packetCount}}
	var out []checkpoint.SegmentRecord
	for part, br := range bounds {
		start, stop := br[0], br[1]
		if stop <= start {
			continue
		}
		partData := data[start*ts.PacketSize : stop*ts.PacketSize]
		rel := segmentPartRelPath(rendition, seq, part)
		recBase := base
		recBase.Partial = true
		recBase.PartIndex = part
		recBase.InputOffset = firstOffset + start*ts.PacketSize
		recBase.InputEnd = firstOffset + stop*ts.PacketSize
		sr, err := store.CommitSegment(rel, bytes.NewReader(partData), recBase)
		if err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, nil
}

func countSegmentRecords(idx *checkpoint.Index) int {
	n := 0
	for _, c := range idx.Chunks {
		for _, s := range c.Segments {
			if !s.Partial {
				n++
			}
		}
	}
	return n
}

func writeSources(out string, sources []fixture.Source) error {
	for _, s := range sources {
		rel := filepath.Join("sources", s.Rendition+".ts")
		full, err := checkpoint.SafePath(out, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, s.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func resetOutput(out string) error {
	if out == "" || !filepath.IsAbs(out) {
		return media.NewError(media.CodeConfig, -1, "output path must be absolute: %q", out)
	}
	for _, p := range []string{"segments", "chunks", "sources"} {
		if err := os.RemoveAll(filepath.Join(out, p)); err != nil {
			return err
		}
	}
	for _, name := range []string{checkpoint.IndexName, checkpoint.JournalName, ConfigFile, ReportFile, ProvenanceFile, "master.m3u8"} {
		_ = os.Remove(filepath.Join(out, name))
	}
	entries, err := os.ReadDir(out)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".m3u8" {
			_ = os.Remove(filepath.Join(out, e.Name()))
		}
	}
	return nil
}

func LoadOptions(out string) (Options, error) {
	var opt Options
	b, err := os.ReadFile(filepath.Join(out, ConfigFile))
	if err != nil {
		return opt, media.WrapError(media.CodeConfig, -1, err, "read %s", ConfigFile)
	}
	if err := json.Unmarshal(b, &opt); err != nil {
		return opt, media.WrapError(media.CodeConfig, -1, err, "parse %s", ConfigFile)
	}
	return opt, nil
}
