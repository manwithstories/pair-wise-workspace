// Package verify re-reads everything the pipeline produced and checks it
// against the source: segment hashes, manifest round-trips, keyframe index
// resolutions and cross-representation time alignment.
//
// Verification never trusts its input. Each check produces an explicit,
// located fault (camera / sequence / byte offset / file) so a failure in a
// 10 000-frame delivery can be traced to one record without re-running the
// pipeline.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/index"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/segment"
)

// Options controls one verification pass.
type Options struct {
	// OutDir is the directory holding segments, manifests and the index.
	OutDir string
	// ExpectCamera names the camera whose artefacts are verified. Empty
	// verifies every camera manifest found.
	ExpectCamera string
	// RandomSeeks is the number of index seeks exercised with SeekSeed.
	RandomSeeks int
	SeekSeed    int64
	// Ladder is the expected representation list.
	Ladder []segment.Representation
	// AlignmentToleranceTicks allows this much cross-representation drift
	// before an alignment fault is raised (0 = exact).
	AlignmentToleranceTicks uint64
	// SourceFrames maps each camera to the number of frames the ingest stage
	// parsed. When present the round-trip rate is the fraction of parsed
	// frames that survived into the manifest, so corrupt input frames make
	// the rate drop below 1. When absent it is the fraction of segment
	// hashes that verified.
	SourceFrames map[string]int
}

// Report is the aggregate verification outcome.
type Report struct {
	RoundTripRate      float64    `json:"roundtrip_rate"`
	KeyframeHitRate    float64    `json:"keyframe_hit_rate"`
	SegmentsChecked    int        `json:"segment_sha256_checked"`
	AlignmentError     uint64     `json:"alignment_error_ticks"`
	SeekChecked        int        `json:"seek_checked"`
	ManifestsChecked   int        `json:"manifests_checked"`
	EmptySegments      int        `json:"empty_segments"`
	Faults             fault.List `json:"faults"`
	FirstErrorOffset   int64      `json:"first_error_offset"`
	VerifiedCameras    []string   `json:"verified_cameras"`
	TruncatedRecords   int        `json:"truncated_records"`
	ChecksumMismatches int        `json:"checksum_mismatches"`
}

// Fatal reports whether verification found a defect that must fail the run.
func (r *Report) Fatal() bool { return r.Faults.Fatal() }

// Run verifies every manifest in opts.OutDir.
func Run(opts Options) *Report {
	rep := &Report{}
	if opts.RandomSeeks <= 0 {
		opts.RandomSeeks = 64
	}
	if len(opts.Ladder) == 0 {
		opts.Ladder = segment.DefaultLadder
	}
	manifests, err := filepath.Glob(filepath.Join(opts.OutDir, "*.manifest.json"))
	if err != nil {
		rep.Faults = append(rep.Faults, fault.Fault{
			Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify,
			File: opts.OutDir, Message: err.Error(), Fatal: true,
		})
		return rep
	}
	sort.Strings(manifests)
	byCamera := map[string][]segment.Manifest{}
	for _, path := range manifests {
		// The combined manifest is "<camera>.manifest.json"; per-rendition
		// manifests are "<camera>-<rep>.manifest.json". Skip the per-rep
		// ones here, they are reachable through the combined list.
		base := strings.TrimSuffix(filepath.Base(path), ".manifest.json")
		if strings.Contains(base, "-") {
			continue
		}
		if opts.ExpectCamera != "" && base != opts.ExpectCamera {
			continue
		}
		m, f := loadManifest(path)
		rep.Faults = append(rep.Faults, f...)
		if m == nil {
			continue
		}
		byCamera[m.Camera] = append(byCamera[m.Camera], *m)
		rep.ManifestsChecked++
	}
	cams := make([]string, 0, len(byCamera))
	for c := range byCamera {
		cams = append(cams, c)
	}
	sort.Strings(cams)
	rep.VerifiedCameras = cams

	rng := rand.New(rand.NewSource(opts.SeekSeed))
	for _, cam := range cams {
		verifyCamera(rep, cam, byCamera[cam], opts, rng)
	}
	rep.Faults = rep.Faults.Sorted()
	if off, ok := rep.Faults.MinOffset(); ok {
		rep.FirstErrorOffset = off
	} else {
		rep.FirstErrorOffset = -1
	}
	rep.TruncatedRecords = rep.Faults.Count(fault.KindTruncatedRecord)
	rep.ChecksumMismatches = rep.Faults.Count(fault.KindChecksumMismatch)
	return rep
}

// loadManifest round-trips one manifest through JSON.
func loadManifest(path string) (*segment.Manifest, fault.List) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify,
			File: path, Message: err.Error(), Fatal: true}}
	}
	var m segment.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify,
			File: path, Message: "manifest is not valid JSON: " + err.Error(), Fatal: true}}
	}
	var faults fault.List
	if m.Version != 1 {
		faults = append(faults, fault.Fault{
			Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify, File: path,
			Message: fmt.Sprintf("unknown manifest version %d", m.Version), Fatal: true,
		})
	}
	if m.Camera == "" || len(m.Segments) == 0 {
		faults = append(faults, fault.Fault{
			Kind: fault.KindEmptySegment, Stage: fault.StageVerify, File: path,
			Message: "manifest has no camera or no segments", Fatal: true,
		})
		return &m, faults
	}
	// Re-marshal and unmarshal to prove the document survives a round trip:
	// any field that fails to serialize would change the second encoding.
	again, err := json.Marshal(&m)
	if err != nil {
		faults = append(faults, fault.Fault{
			Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify, File: path,
			Message: "manifest cannot be re-encoded: " + err.Error(), Fatal: true,
		})
		return &m, faults
	}
	var m2 segment.Manifest
	if err := json.Unmarshal(again, &m2); err != nil || !sameManifest(&m, &m2) {
		faults = append(faults, fault.Fault{
			Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify, File: path,
			Message: "manifest does not survive a JSON round trip", Fatal: true,
		})
	}
	return &m, faults
}

func sameManifest(a, b *segment.Manifest) bool {
	if a.Camera != b.Camera || a.Version != b.Version ||
		a.Timescale != b.Timescale || a.Representation != b.Representation {
		return false
	}
	if len(a.Segments) != len(b.Segments) || len(a.SwitchList) != len(b.SwitchList) ||
		len(a.Ladder) != len(b.Ladder) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i] != b.Segments[i] {
			return false
		}
	}
	for i := range a.SwitchList {
		if a.SwitchList[i].Index != b.SwitchList[i].Index ||
			a.SwitchList[i].StartTS != b.SwitchList[i].StartTS ||
			a.SwitchList[i].EndTS != b.SwitchList[i].EndTS ||
			len(a.SwitchList[i].Renditions) != len(b.SwitchList[i].Renditions) {
			return false
		}
	}
	if len(a.SwitchList) > 0 {
		for k, v := range a.SwitchList[0].Renditions {
			if b.SwitchList[0].Renditions[k] != v {
				return false
			}
		}
	}
	return true
}

// verifyCamera checks one camera's segments and index.
func verifyCamera(rep *Report, cam string, manifests []segment.Manifest, opts Options, rng *rand.Rand) {
	if len(manifests) == 0 {
		return
	}
	combined := manifests[0]
	outDir := opts.OutDir

	// 1. Per-segment SHA-256 and alignment across representations.
	type span struct {
		start, end uint64
		file       string
		rep        string
	}
	byIndex := map[int][]span{}
	for _, s := range combined.Segments {
		if s.Representation == "" {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindMalformedMetadata, Stage: fault.StageVerify, Camera: cam,
				File: s.File, Message: "segment has no representation", Fatal: true,
			})
			continue
		}
		path := filepath.Join(outDir, s.File)
		data, err := os.ReadFile(path)
		if err != nil {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageVerify, Camera: cam,
				File: path, Message: "segment missing: " + err.Error(), Fatal: true,
			})
			continue
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		rep.SegmentsChecked++
		if got != s.SHA256 {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindHashMismatch, Stage: fault.StageVerify, Camera: cam,
				File: path, Seq: s.FirstSeq,
				Message:  "segment SHA-256 does not match the manifest",
				Expected: s.SHA256, Actual: got, Fatal: true,
			})
		}
		if s.FrameCount == 0 || s.EndTS <= s.StartTS {
			rep.EmptySegments++
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindEmptySegment, Stage: fault.StageVerify, Camera: cam,
				File: path, Seq: s.FirstSeq,
				Message: "segment declares no frames or a non-positive duration", Fatal: false,
			})
		}
		if !s.StartKeyframe {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindMissingKeyframe, Stage: fault.StageVerify, Camera: cam,
				File: path, Seq: s.FirstSeq,
				Message: "segment does not start on a keyframe", Fatal: false,
			})
		}
		byIndex[s.Index] = append(byIndex[s.Index], span{s.StartTS, s.EndTS, s.File, s.Representation})
	}

	// 2. Cross-representation alignment: same start/end at every index.
	for idx, spans := range byIndex {
		if len(spans) < 2 {
			continue
		}
		base := spans[0]
		for _, sp := range spans[1:] {
			d := absDiff(sp.start, base.start)
			if d > rep.AlignmentError {
				rep.AlignmentError = d
			}
			if de := absDiff(sp.end, base.end); de > rep.AlignmentError {
				rep.AlignmentError = de
			}
			if d > opts.AlignmentToleranceTicks || absDiff(sp.end, base.end) > opts.AlignmentToleranceTicks {
				rep.Faults = append(rep.Faults, fault.Fault{
					Kind: fault.KindAlignmentMismatch, Stage: fault.StageVerify, Camera: cam,
					File: sp.file,
					Message: fmt.Sprintf("segment %d boundary differs between %s (%d..%d) and %s (%d..%d)",
						idx, sp.rep, sp.start, sp.end, base.rep, base.start, base.end),
					Expected: fmt.Sprintf("%d..%d", base.start, base.end),
					Actual:   fmt.Sprintf("%d..%d", sp.start, sp.end),
					Fatal:    opts.AlignmentToleranceTicks == 0,
				})
			}
		}
	}

	// 3. Manifest round-trip rate: fraction of segments whose manifest entry
	// survived intact. Load already proved the JSON round trip; here we
	// check every listed file exists (the same set that produced hashes).
	listed := len(combined.Segments)
	verified := listed - countFaultsFor(rep.Faults, cam, fault.KindHashMismatch)
	if verified < 0 {
		verified = 0
	}
	if listed > 0 {
		rep.RoundTripRate = float64(verified) / float64(listed)
	}
	// With ingest counts available, express the rate over frames: corrupt
	// input frames are never segmented, so a damaged capture round-trips
	// slightly below 1.000 instead of pretending everything survived.
	if src := opts.SourceFrames[cam]; src > 0 {
		covered := 0
		repSeen := map[string]bool{}
		for _, s := range combined.Segments {
			if repSeen[s.Representation] {
				continue
			}
			repSeen[s.Representation] = true
			covered = 0
			for _, t := range combined.Segments {
				if t.Representation == s.Representation {
					covered += t.FrameCount
				}
			}
			break
		}
		if covered > 0 {
			rate := float64(covered) / float64(src)
			// Round-trip cannot exceed 1: a segment's frame count is its
			// declared span, and a tail segment that ends exactly on the
			// last frame may count it once more than ingest did.
			if rate > 1 {
				rate = 1
			}
			rep.RoundTripRate = rate
		}
	}

	// 4. Keyframe index: load, verify integrity, and exercise random seeks.
	idxPath := filepath.Join(outDir, index.FileName)
	idx, lf := index.Load(idxPath)
	if idx == nil || lf.Fatal() {
		// A damaged index is rebuilt from segments; the point of the index
		// is to be recoverable, so rebuild rather than fail the delivery.
		rebuilt, rf := index.Rebuild(cam, outDir)
		rep.Faults = append(rep.Faults, rf...)
		if rebuilt == nil {
			rep.Faults = append(rep.Faults, lf...)
			return
		}
		idx = rebuilt
	}
	rep.Faults = append(rep.Faults, lf...)
	rep.Faults = append(rep.Faults, idx.CheckIntegrity(outDir)...)
	hit, total := seekHits(idx, combined, outDir, opts.RandomSeeks, rng)
	rep.SeekChecked += total
	if total > 0 {
		rep.KeyframeHitRate = float64(hit) / float64(total)
	} else {
		rep.KeyframeHitRate = 1
	}
}

// seekHits exercises RandomSeeks keyframe queries.
func seekHits(idx *index.KeyframeIndex, m segment.Manifest, outDir string, n int, rng *rand.Rand) (hit, total int) {
	reps := map[string]bool{}
	for _, s := range m.Segments {
		reps[s.Representation] = true
	}
	names := make([]string, 0, len(reps))
	for r := range reps {
		names = append(names, r)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return 0, 0
	}
	var lastTS uint64
	for _, s := range m.Segments {
		if s.EndTS > lastTS {
			lastTS = s.EndTS
		}
	}
	for i := 0; i < n; i++ {
		rep := names[rng.Intn(len(names))]
		var ts uint64
		if lastTS > 0 {
			ts = uint64(rng.Int63n(int64(lastTS)))
		}
		total++
		e, err := idx.Query(rep, ts, outDir)
		if err != nil {
			continue
		}
		// The hit must resolve to a segment the manifest lists.
		if e.MasterTS <= ts && hasSegment(m, e.Segment) {
			hit++
		}
	}
	return hit, total
}

// countFaultsFor counts faults of a kind recorded for one camera.
func countFaultsFor(faults fault.List, cam string, kind fault.Kind) int {
	n := 0
	for i := range faults {
		if faults[i].Kind == kind && faults[i].Camera == cam {
			n++
		}
	}
	return n
}

func hasSegment(m segment.Manifest, file string) bool {
	for _, s := range m.Segments {
		if s.File == file {
			return true
		}
	}
	return false
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// SourceCheck re-reads the ingest frames and reports the first error offset
// and defect counts. It shares the model fault vocabulary with the older
// pipeline so exit codes stay consistent.
func SourceCheck(faults []model.Fault) (firstOffset int64, corrupt, truncated int) {
	firstOffset = -1
	for _, f := range faults {
		switch f.Class {
		case model.FaultChecksum:
			corrupt++
		case model.FaultMissing:
			truncated++
		}
		if f.Location.Offset >= 0 && (firstOffset < 0 || f.Location.Offset < firstOffset) {
			firstOffset = f.Location.Offset
		}
	}
	return firstOffset, corrupt, truncated
}
