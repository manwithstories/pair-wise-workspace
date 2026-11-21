// Package segment cuts the verified timeline into per-bitrate media segments
// on keyframe boundaries and writes the delivery manifests that accompany
// them.
//
// Segmentation rules:
//
//   - every representation (360p/720p/1080p) is cut at exactly the same
//     keyframe timestamps, so a player can switch renditions at any segment
//     boundary;
//   - a segment starts on a keyframe and never starts on a missing one: if a
//     keyframe is absent the segmenter closes the previous segment, records a
//     missing-keyframe fault and resumes at the next available keyframe;
//   - each segment's bytes are written to a temporary file and atomically
//     renamed, so an interrupted write never leaves a half segment behind.
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/pipeline"
)

// Representation is one output bitrate ladder rung.
type Representation struct {
	Name    string `json:"name"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Bitrate int    `json:"bitrate_bps"`
}

// DefaultLadder is the 360p/720p/1080p ladder the acceptance run requires.
var DefaultLadder = []Representation{
	{Name: "360p", Width: 640, Height: 360, Bitrate: 800_000},
	{Name: "720p", Width: 1280, Height: 720, Bitrate: 2_500_000},
	{Name: "1080p", Width: 1920, Height: 1080, Bitrate: 5_000_000},
}

// FormatLadderNames renders a ladder as a comma-separated name list.
func FormatLadderNames(l []Representation) string {
	names := make([]string, len(l))
	for i, r := range l {
		names[i] = r.Name
	}
	return strings.Join(names, ",")
}

// ParseLadder turns a "360p,720p,1080p" list into representations.
func ParseLadder(spec string) ([]Representation, error) {
	if strings.TrimSpace(spec) == "" {
		return append([]Representation(nil), DefaultLadder...), nil
	}
	byName := map[string]Representation{}
	for _, r := range DefaultLadder {
		byName[r.Name] = r
	}
	var out []Representation
	seen := map[string]bool{}
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		r, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("segment: unknown representation %q (want 360p, 720p or 1080p)", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("segment: representation %q listed twice", name)
		}
		seen[name] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("segment: empty representation list")
	}
	return out, nil
}

// Segment is one cut of one representation.
type Segment struct {
	Representation string `json:"representation"`
	Index          int    `json:"index"`
	File           string `json:"file"`
	StartTS        uint64 `json:"start_ts"` // master 90kHz
	EndTS          uint64 `json:"end_ts"`   // master 90kHz, exclusive
	DurationTicks  uint64 `json:"duration_ticks"`
	FrameCount     int    `json:"frame_count"`
	FirstSeq       uint64 `json:"first_seq"`
	LastSeq        uint64 `json:"last_seq"`
	Size           int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
	// StartKeyframe is false when the segment was opened by a forced cut at
	// a missing keyframe; such segments are not independently decodable.
	StartKeyframe bool `json:"start_keyframe"`
}

// Manifest is the delivery list for one representation, or the combined
// switchable list when Representation is empty.
type Manifest struct {
	Version         int              `json:"version"`
	Camera          string           `json:"camera"`
	Representation  string           `json:"representation,omitempty"`
	Ladder          []Representation `json:"ladder,omitempty"`
	Timescale       uint64           `json:"timescale"`
	Segments        []Segment        `json:"segments"`
	TotalDurationTS uint64           `json:"total_duration_ts"`
	KeyframeCount   int              `json:"keyframe_count"`
	// SwitchList groups the segment files of every representation by segment
	// number so a player can switch renditions at a boundary. It is only
	// populated on the combined per-camera manifest.
	SwitchList []SwitchPoint `json:"switch_points,omitempty"`
}

// SwitchPoint names one switchable boundary and the file of every
// representation that starts there.
type SwitchPoint struct {
	Index      int               `json:"index"`
	StartTS    uint64            `json:"start_ts"`
	EndTS      uint64            `json:"end_ts"`
	Renditions map[string]string `json:"renditions"`
}

// Config controls one segmenting pass.
type Config struct {
	// Root is the run directory; output goes to Root/output.
	Root string
	// Ladder lists the representations.
	Ladder []Representation
	// TargetDuration is the minimum segment length in master ticks. A cut is
	// taken at the first keyframe at or after this point.
	TargetDuration uint64
	// KeyEvery is the expected keyframe cadence in frames (0 disables the
	// cadence check).
	KeyEvery uint32
}

// Options carries the shared sink used by the pipeline integration.
type Options struct {
	Config Config
	// Committed receives each segment after its file has been renamed.
	Committed func(Segment)
}

// Result summarises one camera's segmentation.
type Result struct {
	Camera         string
	Segments       []Segment
	Manifests      []string
	Faults         fault.List
	SHA256Checked  int
	MissingKeys    int
	EmptySegments  int
	AlignmentError uint64 // max |end_i - start_{i+1}| across representations
}

// Cut partitions frames into segment boundaries.
//
// A cut is only ever placed on a keyframe: starting at the current segment
// origin, the first keyframe whose master timestamp is at least target ticks
// later becomes the next boundary. If a stream has no further keyframe the
// tail becomes the final segment. With a positive keyEvery the expected
// keyframe cadence is checked, and every absent keyframe is reported with its
// sequence and byte offset while segmentation continues with the remaining
// verifiable frames.
func Cut(frames []pipeline.Item, target uint64, keyEvery uint32) (bounds []int, keyed []bool, faults fault.List) {
	if len(frames) == 0 {
		return nil, nil, nil
	}
	if target == 0 {
		target = 90000 // one second on the master clock
	}

	// Expected-cadence check: a frame that should have been a keyframe but
	// is not is an explicit, located gap.
	if keyEvery > 0 {
		k := uint64(keyEvery)
		for _, f := range frames {
			if !f.Keyframe && f.Seq%k == 0 {
				faults = append(faults, fault.Fault{
					Kind: fault.KindMissingKeyframe, Stage: fault.StageSegment,
					Camera: f.Camera, Seq: f.Seq, Offset: f.Offset,
					Message:  fmt.Sprintf("keyframe missing at expected cadence every %d frames", keyEvery),
					Expected: "keyframe", Actual: "inter frame",
				})
			}
		}
	}

	bounds = append(bounds, 0)
	keyed = append(keyed, frames[0].Keyframe)
	if !frames[0].Keyframe {
		faults = append(faults, fault.Fault{
			Kind: fault.KindMissingKeyframe, Stage: fault.StageSegment,
			Camera: frames[0].Camera, Seq: frames[0].Seq, Offset: frames[0].Offset,
			Message: "stream does not start on a keyframe; first segment is not independently decodable",
		})
	}

	start := 0
	for i := start + 1; i < len(frames); i++ {
		if frames[i].MasterTS-frames[start].MasterTS < target {
			continue
		}
		if !frames[i].Keyframe {
			continue // never cut on an inter frame
		}
		bounds = append(bounds, i)
		keyed = append(keyed, true)
		start = i
	}
	return bounds, keyed, faults
}

// SegmentAll partitions every camera independently but cuts at the same
// master timestamps, then writes the files and manifests.
func SegmentAll(items []pipeline.Item, opts Options) ([]Result, fault.List) {
	byCamera := map[string][]pipeline.Item{}
	for _, it := range items {
		byCamera[it.Camera] = append(byCamera[it.Camera], it)
	}
	cams := make([]string, 0, len(byCamera))
	for c := range byCamera {
		cams = append(cams, c)
	}
	sort.Strings(cams)

	outDir := filepath.Join(opts.Config.Root, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageSegment,
			Message: err.Error(), Fatal: true}}
	}
	ladder := opts.Config.Ladder
	if len(ladder) == 0 {
		ladder = DefaultLadder
	}

	var results []Result
	var allFaults fault.List
	for _, cam := range cams {
		res, fs := segmentCamera(cam, byCamera[cam], outDir, ladder, opts)
		results = append(results, res)
		allFaults = append(allFaults, fs...)
	}
	// Alignment is required between the representations of one camera (a
	// player switching bitrate must land on an identical time boundary);
	// different cameras legitimately have different frame cadences and thus
	// different cut instants.
	return results, allFaults.Sorted()
}

// segmentCamera writes one segment per representation for each cut.
func segmentCamera(cam string, frames []pipeline.Item, outDir string, ladder []Representation, opts Options) (Result, fault.List) {
	res := Result{Camera: cam}
	bounds, keyed, cutFaults := Cut(frames, opts.Config.TargetDuration, opts.Config.KeyEvery)
	res.Faults = append(res.Faults, cutFaults...)
	res.MissingKeys = cutFaults.Count(fault.KindMissingKeyframe)
	if len(frames) == 0 {
		return res, res.Faults
	}

	// Determine the segment ranges: bounds[i] to bounds[i+1] (last runs to
	// the end of the stream).
	edges := append(append([]int(nil), bounds...), len(frames))
	for repIdx, rep := range ladder {
		for i := 0; i < len(edges)-1; i++ {
			lo, hi := edges[i], edges[i+1]
			if hi <= lo {
				res.EmptySegments++
				res.Faults = append(res.Faults, fault.Fault{
					Kind: fault.KindEmptySegment, Stage: fault.StageSegment,
					Camera: cam, Seq: frames[lo].Seq, Offset: frames[lo].Offset,
					Message: "segment range is empty", Fatal: false,
				})
				continue
			}
			// EndTS is exclusive: the next segment's start, or one frame
			// period past the last frame for the tail segment.
			endTS := frames[hi-1].MasterTS
			if hi < len(frames) {
				endTS = frames[hi].MasterTS
			} else if hi-lo > 1 {
				endTS += frames[hi-1].MasterTS - frames[hi-2].MasterTS
			} else {
				endTS += 1
			}
			seg := Segment{
				Representation: rep.Name,
				Index:          i,
				StartTS:        frames[lo].MasterTS,
				EndTS:          endTS,
				DurationTicks:  endTS - frames[lo].MasterTS,
				FrameCount:     hi - lo,
				FirstSeq:       frames[lo].Seq,
				LastSeq:        frames[hi-1].Seq,
				StartKeyframe:  keyed[i],
			}
			name := fmt.Sprintf("%s-%s-%05d.seg", cam, rep.Name, i)
			path := filepath.Join(outDir, name)
			size, digest, err := writeAtomic(path, frames[lo:hi], rep)
			if err != nil {
				res.Faults = append(res.Faults, fault.Fault{
					Kind: fault.KindWriteFailure, Stage: fault.StageSegment,
					Camera: cam, Seq: frames[lo].Seq, Offset: frames[lo].Offset,
					File: path, Message: err.Error(), Fatal: true,
				})
				return res, res.Faults
			}
			seg.File = name
			seg.Size = size
			seg.SHA256 = digest
			res.SHA256Checked++
			res.Segments = append(res.Segments, seg)
			if opts.Committed != nil {
				opts.Committed(seg)
			}
		}
		// Write one manifest per representation.
		m := Manifest{
			Version: 1, Camera: cam, Representation: rep.Name,
			Timescale: 90000, Ladder: ladder,
		}
		for _, s := range res.Segments {
			if s.Representation == rep.Name {
				m.Segments = append(m.Segments, s)
			}
		}
		if n := len(m.Segments); n > 0 {
			m.TotalDurationTS = m.Segments[n-1].EndTS - m.Segments[0].StartTS
		}
		m.KeyframeCount = len(m.Segments)
		path := filepath.Join(outDir, fmt.Sprintf("%s-%s.manifest.json", cam, rep.Name))
		if err := writeManifestAtomic(path, m); err != nil {
			res.Faults = append(res.Faults, fault.Fault{
				Kind: fault.KindWriteFailure, Stage: fault.StageSegment,
				Camera: cam, File: path, Message: err.Error(), Fatal: true,
			})
			return res, res.Faults
		}
		res.Manifests = append(res.Manifests, path)
		_ = repIdx
	}

	// The combined switchable manifest lists every representation side by
	// side; its segment entries are grouped by representation index.
	combined := Manifest{Version: 1, Camera: cam, Timescale: 90000, Ladder: ladder}
	for _, s := range res.Segments {
		combined.Segments = append(combined.Segments, s)
	}
	if len(combined.Segments) > 0 {
		combined.TotalDurationTS = combined.Segments[len(combined.Segments)-1].EndTS -
			combined.Segments[0].StartTS
	}
	combined.KeyframeCount = len(bounds)
	combined.SwitchList = buildSwitchList(res.Segments)
	path := filepath.Join(outDir, fmt.Sprintf("%s.manifest.json", cam))
	if err := writeManifestAtomic(path, combined); err != nil {
		res.Faults = append(res.Faults, fault.Fault{
			Kind: fault.KindWriteFailure, Stage: fault.StageSegment,
			Camera: cam, File: path, Message: err.Error(), Fatal: true,
		})
		return res, res.Faults
	}
	res.Manifests = append(res.Manifests, path)
	// Alignment within the camera: all representations of segment i must
	// share start/end timestamps.
	res.AlignmentError = intraAlignment(res.Segments)
	if res.AlignmentError > 0 {
		res.Faults = append(res.Faults, fault.Fault{
			Kind: fault.KindAlignmentMismatch, Stage: fault.StageSegment, Camera: cam,
			Message:  fmt.Sprintf("representations disagree on segment boundaries by %d ticks", res.AlignmentError),
			Expected: "0", Actual: fmt.Sprintf("%d", res.AlignmentError),
		})
	}
	return res, res.Faults
}

// intraAlignment returns the largest |end-start| mismatch between the same
// segment index across representations.
func intraAlignment(segs []Segment) uint64 {
	type span struct{ start, end uint64 }
	byIdx := map[int]span{}
	var worst uint64
	for _, s := range segs {
		sp, ok := byIdx[s.Index]
		if !ok {
			byIdx[s.Index] = span{s.StartTS, s.EndTS}
			continue
		}
		if s.StartTS != sp.start {
			d := diffU64(s.StartTS, sp.start)
			if d > worst {
				worst = d
			}
		}
		if s.EndTS != sp.end {
			d := diffU64(s.EndTS, sp.end)
			if d > worst {
				worst = d
			}
		}
	}
	return worst
}

func diffU64(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// writeAtomic writes a segment payload to a temporary file and renames it.
// Segment content is a deterministic header plus one descriptor per frame:
// this is a demonstrable media container stand-in whose bytes are stable and
// hashable, not a re-encoded codec stream.
func writeAtomic(path string, frames []pipeline.Item, rep Representation) (int64, string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".seg-*")
	if err != nil {
		return 0, "", err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	w := io.MultiWriter(tmp, h)

	// The header records the master-clock timescale so a reader can rescale
	// every frame line's timestamp unambiguously.
	header := fmt.Sprintf("PAIRFLOWSEG1 rep=%s %dx%d %dbps frames=%d timescale=90000\n",
		rep.Name, rep.Width, rep.Height, rep.Bitrate, len(frames))
	if _, err := io.WriteString(w, header); err != nil {
		tmp.Close()
		return 0, "", err
	}
	buf := make([]byte, 0, 64)
	for _, f := range frames {
		buf = buf[:0]
		// Field order: master timestamp (90 kHz, the canonical timing domain),
		// RTP sequence, source frame sequence, source byte offset. The
		// representation timestamp is derived from the master one by any
		// consumer that needs the 10 MHz output scale, so the file never
		// carries two competing notions of "the time".
		buf = append(buf, fmt.Sprintf("%016x %08x %d %d\n",
			f.MasterTS, f.RTPSeq, f.Seq, f.Offset)...)
		if _, err := w.Write(buf); err != nil {
			tmp.Close()
			return 0, "", err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return 0, "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, "", err
	}
	tmpName = ""
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	return info.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

func writeManifestAtomic(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// buildSwitchList groups segments by index into switchable boundaries.
func buildSwitchList(segs []Segment) []SwitchPoint {
	byIdx := map[int]*SwitchPoint{}
	var order []int
	for _, s := range segs {
		sp, ok := byIdx[s.Index]
		if !ok {
			sp = &SwitchPoint{Index: s.Index, StartTS: s.StartTS, EndTS: s.EndTS,
				Renditions: map[string]string{}}
			byIdx[s.Index] = sp
			order = append(order, s.Index)
		}
		sp.Renditions[s.Representation] = s.File
	}
	sort.Ints(order)
	out := make([]SwitchPoint, 0, len(order))
	for _, i := range order {
		out = append(out, *byIdx[i])
	}
	return out
}

// TotalFrames sums the frame counts of every segment of one representation.
func (r Result) TotalFrames(rep string) int {
	n := 0
	for _, s := range r.Segments {
		if s.Representation == rep {
			n += s.FrameCount
		}
	}
	return n
}

// Alignment returns the largest cross-representation boundary mismatch.
func (r Result) Alignment() uint64 { return r.AlignmentError }

// SegmentCount returns how many segments one representation has.
func (r Result) SegmentCount(rep string) int {
	n := 0
	for _, s := range r.Segments {
		if s.Representation == rep {
			n++
		}
	}
	return n
}
