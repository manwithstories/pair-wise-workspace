package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"relaycheck/internal/demux"
	"relaycheck/internal/provenance"
	"relaycheck/internal/recovery"
	"relaycheck/internal/remux"
)

// VerifyConfig configures an independent verification pass.
type VerifyConfig struct {
	Input     string
	Output    string
	IndexPath string
}

// Verify independently re-checks the provenance chain: input digests,
// per-segment payload hashes, sample tables, decode-time monotonicity and
// the committed chunk chain. Any mismatch is a verification failure.
func Verify(cfg VerifyConfig) (*Report, error) {
	_, files, err := listInputs(cfg.Input)
	if err != nil {
		return nil, err
	}
	initData, err := os.ReadFile(filepath.Join(cfg.Input, "init.mp4"))
	if err != nil {
		return nil, err
	}
	if _, fault := demux.ParseInit(initData); fault != nil {
		return nil, fmt.Errorf("init: %s", fault)
	}
	digest, err := inputDigest(cfg.Input, files)
	if err != nil {
		return nil, err
	}
	idx, err := recovery.Open(cfg.IndexPath)
	if err != nil {
		return nil, err
	}
	defer idx.Close()
	if idx.Header == nil {
		return nil, fmt.Errorf("verification failed: index header missing")
	}
	if idx.Header.ParamsDigest != recovery.ParamsDigest(digest, idx.Header.ChunkSize) {
		return nil, fmt.Errorf("verification failed: index does not match input digest")
	}
	if idx.Done == nil {
		return nil, fmt.Errorf("verification failed: run incomplete (no done record)")
	}
	var maxSeq uint32
	for _, f := range files {
		if f.pos > maxSeq {
			maxSeq = f.pos
		}
	}
	chunkCount := int((maxSeq + uint32(idx.Header.ChunkSize) - 1) / uint32(idx.Header.ChunkSize))
	for c := 1; c <= chunkCount; c++ {
		if _, ok := idx.Chunks[c]; !ok {
			return nil, fmt.Errorf("verification failed: chunk %d not committed", c)
		}
	}
	trace, err := provenance.Open(filepath.Join(cfg.Output, "provenance"))
	if err != nil {
		return nil, err
	}
	traces, err := trace.Load()
	if err != nil {
		return nil, err
	}
	outInit, err := os.ReadFile(filepath.Join(cfg.Output, "init.mp4"))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(initData, outInit) {
		return nil, fmt.Errorf("verification failed: output init differs from input init")
	}
	rep, err := LoadReport(cfg.Output)
	if err != nil {
		return nil, fmt.Errorf("verification failed: %v", err)
	}
	if rep.ResultsDigest != idx.Done.ResultsDigest {
		return nil, fmt.Errorf("verification failed: report digest != committed digest")
	}
	if rep.ResultsDigest != resultsDigest(idx, chunkCount) {
		return nil, fmt.Errorf("verification failed: chunk chain digest mismatch")
	}
	if err := checkOutputSet(cfg.Output, traces); err != nil {
		return nil, err
	}
	if err := checkTraces(cfg, traces); err != nil {
		return nil, err
	}
	return rep, nil
}

// checkOutputSet ensures the output holds exactly the accepted segments.
func checkOutputSet(dir string, traces []provenance.SegmentTrace) error {
	accepted := map[string]bool{}
	for _, tr := range traces {
		if tr.Decision == provenance.Accepted {
			accepted[fmt.Sprintf("seg_%06d.m4s", tr.Seq)] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !segNameRe.MatchString(e.Name()) {
			continue
		}
		if !accepted[e.Name()] {
			return fmt.Errorf("verification failed: unexpected output segment %s", e.Name())
		}
		delete(accepted, e.Name())
	}
	if len(accepted) > 0 {
		return fmt.Errorf("verification failed: %d accepted segments missing from output", len(accepted))
	}
	return nil
}

// checkTraces recomputes payload hashes and sample tables from the input
// and output files and compares them against the provenance records.
func checkTraces(cfg VerifyConfig, traces []provenance.SegmentTrace) error {
	prevFirst := map[uint32]int64{}
	prevDur := map[uint32]int64{}
	var compared, matched int
	for _, tr := range traces {
		if tr.Decision != provenance.Accepted {
			continue
		}
		inSeg, inSamples, err := parseFile(filepath.Join(cfg.Input, tr.File), tr.Seq)
		if err != nil {
			return err
		}
		outName := fmt.Sprintf("seg_%06d.m4s", tr.Seq)
		outSeg, outSamples, err := parseFile(filepath.Join(cfg.Output, outName), tr.Seq)
		if err != nil {
			return err
		}
		if outSeg.Sequence != tr.Seq {
			return fmt.Errorf("verification failed: seq %d output mfhd=%d", tr.Seq, outSeg.Sequence)
		}
		if len(inSeg.Traf) != len(outSeg.Traf) || len(inSeg.Traf) != len(tr.Tracks) {
			return fmt.Errorf("verification failed: seq %d track count changed", tr.Seq)
		}
		for i := range tr.Tracks {
			tt := &tr.Tracks[i]
			in := samplesOf(inSeg, inSamples, tt.TrackID)
			out := samplesOf(outSeg, outSamples, tt.TrackID)
			if in == nil || out == nil {
				return fmt.Errorf("verification failed: seq %d track %d missing", tr.Seq, tt.TrackID)
			}
			inHash := hashOf(in)
			outHash := hashOf(out)
			compared++
			if inHash == outHash && tt.PayloadSHA == tt.OutputSHA && tt.PayloadSHA == inHash {
				matched++
			}
			if err := sameSampleTable(in, out); err != nil {
				return fmt.Errorf("verification failed: seq %d track %d: %v", tr.Seq, tt.TrackID, err)
			}
			first := int64(outSeg.Traf[indexOfTraf(outSeg, tt.TrackID)].TFDT)
			if first != tt.FirstDTS {
				return fmt.Errorf("verification failed: seq %d track %d tfdt != trace", tr.Seq, tt.TrackID)
			}
			if p, ok := prevFirst[tt.TrackID]; ok && first < p+prevDur[tt.TrackID] {
				return fmt.Errorf("verification failed: seq %d track %d DTS not monotonic", tr.Seq, tt.TrackID)
			}
			prevFirst[tt.TrackID] = first
			prevDur[tt.TrackID] = tt.Duration
		}
	}
	if compared == 0 || matched != compared {
		return fmt.Errorf("verification failed: payload hash match %d/%d", matched, compared)
	}
	return nil
}

func parseFile(path string, seq uint32) (*demux.Segment, [][]demux.Sample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	seg, fault := demux.ParseSegment(data, seq)
	if fault != nil {
		return nil, nil, fmt.Errorf("%s: %s", path, fault)
	}
	samples, fault := seg.ExtractAll(seq)
	if fault != nil {
		return nil, nil, fmt.Errorf("%s: %s", path, fault)
	}
	return seg, samples, nil
}

func indexOfTraf(seg *demux.Segment, trackID uint32) int {
	for i := range seg.Traf {
		if seg.Traf[i].TrackID == trackID {
			return i
		}
	}
	return -1
}

func samplesOf(seg *demux.Segment, all [][]demux.Sample, trackID uint32) []demux.Sample {
	i := indexOfTraf(seg, trackID)
	if i < 0 {
		return nil
	}
	return all[i]
}

func hashOf(samples []demux.Sample) string {
	parts := make([][]byte, len(samples))
	for i, s := range samples {
		parts[i] = s.Data
	}
	sum := remux.HashPayload(parts)
	return fmt.Sprintf("%x", sum[:])
}

// sameSampleTable proves the remux preserved every per-sample attribute.
func sameSampleTable(a, b []demux.Sample) error {
	if len(a) != len(b) {
		return fmt.Errorf("sample count %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Duration != b[i].Duration || a[i].Size != b[i].Size ||
			a[i].Flags != b[i].Flags || a[i].CTO != b[i].CTO {
			return fmt.Errorf("sample %d attributes changed", i)
		}
	}
	return nil
}
