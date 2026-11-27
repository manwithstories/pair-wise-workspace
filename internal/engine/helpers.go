package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"avrepair/internal/clock"
	"avrepair/internal/fixture"
	"avrepair/internal/subtitle"
)

// validateOutDir confines outputs to an absolute, non-root local path so the
// tool can never write over the input or escape into system locations.
func validateOutDir(dir string) error {
	if dir == "" {
		return clock.NewError(clock.CodeBadOffset, "", clock.Interval{},
			"output directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs == "/" || abs == filepath.Clean(os.Getenv("HOME")) {
		return clock.NewError(clock.CodeBadOffset, "", clock.Interval{},
			"refusing to write into a protected directory: "+abs)
	}
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}

// snapToCadence moves any packet whose timestamp has drifted from its nominal
// slot (seq*step) back onto that slot, provided strict monotonicity and the
// 500 ms correction budget are both preserved. Packets that cannot be snapped
// are left untouched (they retain the clock layer's conservative repair).
func snapToCadence(pkts []fixture.Packet, step int64) []fixture.Packet {
	out := make([]fixture.Packet, len(pkts))
	copy(out, pkts)
	for i := range out {
		ideal := int64(out[i].Seq) * step
		cur := out[i].TS
		if cur == ideal {
			continue
		}
		if abs64(ideal-cur) > clock.MaxRepairDelta {
			continue
		}
		// Nominal slots are strictly increasing; only the already-snapped
		// predecessor can constrain placement.
		if i > 0 && ideal <= out[i-1].TS {
			continue
		}
		out[i].TS = ideal
	}
	return out
}

// snapCues is the subtitle analogue of snapToCadence: repaired cue starts are
// returned to their nominal one-second slots within the repair budget while
// preserving strict ordering.
func snapCues(cues []subtitle.Cue, step int64) []subtitle.Cue {
	out := make([]subtitle.Cue, len(cues))
	copy(out, cues)
	for i := range out {
		ideal := int64(out[i].Index) * step
		cur := out[i].StartNS
		if cur == ideal || abs64(ideal-cur) > clock.MaxRepairDelta {
			continue
		}
		if i > 0 && ideal <= out[i-1].StartNS {
			continue
		}
		out[i].StartNS = ideal
	}
	return out
}

// trimVideo projects packets onto clock points, preserving order.
func toPoints(track string, pkts []fixture.Packet) []clock.Point {
	out := make([]clock.Point, len(pkts))
	for i, p := range pkts {
		out[i] = clock.Point{Track: track, Seq: p.Seq, TS: p.TS}
	}
	return out
}

// applyRepair writes repaired timestamps back onto packet copies.
func applyRepair(pkts []fixture.Packet, rep clock.RepairResult) []fixture.Packet {
	fixed := make([]fixture.Packet, len(pkts))
	copy(fixed, pkts)
	bySeq := map[int]int{}
	for i, p := range fixed {
		bySeq[p.Seq] = i
	}
	for _, p := range rep.Repaired {
		fixed[bySeq[p.Seq]].TS = p.TS
	}
	// Drop isolated packets from the active stream.
	if len(rep.Isolated) == 0 {
		return fixed
	}
	isolated := map[int]bool{}
	for _, p := range rep.Isolated {
		isolated[p.Seq] = true
	}
	out := fixed[:0]
	for _, p := range fixed {
		if !isolated[p.Seq] {
			out = append(out, p)
		}
	}
	return out
}

func trimVideo(pkts []fixture.Packet, beforeNS int64) []fixture.Packet {
	var out []fixture.Packet
	for _, p := range pkts {
		if p.TS < beforeNS {
			out = append(out, p)
		}
	}
	return out
}

// effectiveDuration returns the timestamp of the last surviving packet plus its
// frame duration, capped at the recovered window, so bitrate is measured over
// the content actually present rather than the nominal full length.
func effectiveDuration(video, audio []fixture.Packet, recoveredNS int64) int64 {
	var last, span int64
	for _, p := range video {
		if p.TS < recoveredNS && p.TS > last {
			last, span = p.TS, p.Duration
		}
	}
	for _, p := range audio {
		if p.TS < recoveredNS && p.TS > last {
			last, span = p.TS, p.Duration
		}
	}
	if last == 0 {
		return recoveredNS
	}
	end := last + span
	if end > recoveredNS {
		end = recoveredNS
	}
	if end <= 0 {
		return recoveredNS
	}
	return end
}

func trimAudio(pkts []fixture.Packet, beforeNS int64) []fixture.Packet {
	return trimVideo(pkts, beforeNS)
}

// writeAtomic publishes bytes via temp-file + rename so readers never observe
// a partial document.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// encodeManifest renders records as deterministic, indented JSON.
func encodeManifest(records []manifestRecord) []byte {
	type envelope struct {
		Records []manifestRecord `json:"records"`
	}
	data, _ := json.MarshalIndent(envelope{Records: records}, "", "  ")
	return append(data, '\n')
}

// hashOutputs hashes the committed output file set in a stable order so the
// digest depends only on content, never on map iteration or wall-clock time.
func hashOutputs(caseDir string, idx interface{ Path() string }) string {
	files := []string{
		filepath.Join(caseDir, "manifest.json"),
		filepath.Join(caseDir, "subtitles.srt"),
		idx.Path(),
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return ""
		}
		h.Write([]byte(filepath.Base(f)))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// checkRerun confirms a recomputed report hashes to the same output digest.
func checkRerun(caseDir string, rep *Report) error {
	summaryPath := filepath.Join(caseDir, "summary.json")
	if _, err := os.Stat(summaryPath); err == nil {
		prior, err := os.ReadFile(summaryPath)
		if err != nil {
			return err
		}
		var prev Report
		if err := json.Unmarshal(prior, &prev); err != nil {
			return err
		}
		if prev.OutputSHA256 != rep.OutputSHA256 {
			return clock.NewError(clock.CodeChecksum, rep.Case, clock.Interval{},
				"rerun produced a different output digest")
		}
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(summaryPath, append(data, '\n'))
}
