// Package pipeline wires the scanner, normalizer, manifest aligner and
// checksum aggregator into the scheduler DAG, and generates the
// deterministic self-contained input assets.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"archivecheck/internal/archive"
	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/fixture"
	"archivecheck/internal/isobmff"
	"archivecheck/internal/manifest"
	"archivecheck/internal/normalize"
	"archivecheck/internal/scheduler"
)

// Params are the run parameters; every field feeds the params hash.
type Params struct {
	DurationSec  int64  `json:"duration_sec"`
	BitratesKbps []int  `json:"bitrates_kbps"`
	Inject       string `json:"inject"`
	ColorPolicy  string `json:"color_policy"`
	SegmentSec   int64  `json:"segment_sec"`
}

// Hash returns the canonical parameter hash used for resume validation.
func (p Params) Hash() string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ParseInject parses "truncate:track<N>:sample<M>".
func ParseInject(s string) (track int, sample int64, err error) {
	if s == "" {
		return 0, 0, nil
	}
	var kind string
	if _, err := fmt.Sscanf(s, "%[^:]:track%d:sample%d", &kind, &track, &sample); err != nil {
		return 0, 0, fmt.Errorf("invalid --inject %q (want truncate:trackN:sampleM)", s)
	}
	if kind != "truncate" || track < 1 || sample < 1 {
		return 0, 0, fmt.Errorf("invalid --inject %q (want truncate:trackN:sampleM)", s)
	}
	return track, sample, nil
}

// FixtureSpec builds the deterministic generator spec for one representation:
// 25 fps video with 2 s GOPs and a 2 s non-zero media start via edit list,
// 48 kHz stereo audio with its own 2 s media offset.
func FixtureSpec(p Params, rep int) (fixture.Spec, error) {
	injTrack, injSample, err := ParseInject(p.Inject)
	if err != nil {
		return fixture.Spec{}, err
	}
	d := p.DurationSec
	spec := fixture.Spec{
		MovieTimescale: 1000,
		Tracks: []fixture.TrackSpec{
			{
				Handler: "vide", Timescale: 90000,
				SampleCount: uint32(d*25 + 50), SampleDelta: 3600,
				KeyframeEvery: 50, Width: 1920, Height: 1080,
				Color: &fixture.ColorBT709,
				Edits: []clock.Edit{{MediaTime: 180000, Duration: d * 1000}},
			},
			{
				Handler: "soun", Timescale: 48000,
				SampleCount: uint32((96000 + d*48000 + 1023) / 1024), SampleDelta: 1024,
				Channels: 2, SampleRate: 48000,
				Edits: []clock.Edit{{MediaTime: 96000, Duration: d * 1000}},
			},
		},
	}
	if injTrack == 2 {
		spec.Tracks[1].TruncateStszTo = int(injSample) - 1
	} else if injTrack == 1 {
		spec.Tracks[0].TruncateStszTo = int(injSample) - 1
	}
	return spec, nil
}

// EnsureInputs (re)generates the representation MP4 files under dir.
// Generation is deterministic, so regenerating is idempotent.
func EnsureInputs(dir string, p Params) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var paths []string
	for i, br := range p.BitratesKbps {
		spec, err := FixtureSpec(p, i)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, fmt.Sprintf("rep_%dk.mp4", br))
		if err := fixture.Generate(path, spec); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// TrackOut is the serializable scan view of one track.
type TrackOut struct {
	Number       int                `json:"number"`
	Kind         string             `json:"kind"`
	Timescale    int64              `json:"timescale"`
	Declared     int64              `json:"declared"`
	Valid        int64              `json:"valid"`
	KeyframeNums []int64            `json:"keyframe_samples,omitempty"`
	Keyframes    []clock.Time       `json:"keyframes,omitempty"`
	Duration     clock.Time         `json:"duration"`
	Channels     int                `json:"channels,omitempty"`
	SampleRate   int                `json:"sample_rate,omitempty"`
	Width        int                `json:"width,omitempty"`
	Height       int                `json:"height,omitempty"`
	Color        *isobmff.ColorInfo `json:"color,omitempty"`
}

// RepOut is one scanned representation.
type RepOut struct {
	Name        string    `json:"name"`
	BitrateKbps int       `json:"bitrate_kbps"`
	Video       *TrackOut `json:"video"`
	Audio       *TrackOut `json:"audio"`
}

// ScanOut is the scan node output.
type ScanOut struct {
	Reps   []RepOut   `json:"reps"`
	Faults fault.List `json:"faults,omitempty"`
}

// NormOut is the normalize node output.
type NormOut struct {
	Audio []normalize.AudioDesc    `json:"audio"`
	Color *normalize.ColorDecision `json:"color"`
}

// SegOut is the segment node output.
type SegOut struct {
	Manifest *manifest.Manifest `json:"manifest"`
}

// FinalOut is the checksum node output and the run's verdict.
type FinalOut struct {
	Checksum          string     `json:"checksum"`
	Segments          int        `json:"segments"`
	Representations   int        `json:"representations"`
	MaxBoundarySkewMs int64      `json:"max_boundary_skew_ms"`
	Faults            fault.List `json:"faults,omitempty"`
}

func trackOut(si *isobmff.SampleIndex, video bool) (*TrackOut, error) {
	tr := si.Track
	to := &TrackOut{
		Number: tr.Number, Timescale: tr.Timescale,
		Declared: si.Declared, Valid: si.Valid,
		Channels: tr.Channels, SampleRate: tr.SampleRate,
		Width: tr.Width, Height: tr.Height, Color: tr.Color,
	}
	if video {
		to.Kind = "video"
		pts, err := si.KeyframePresentationTimes()
		if err != nil {
			return nil, err
		}
		for _, k := range si.Keyframes {
			if k > si.Valid {
				break
			}
			to.KeyframeNums = append(to.KeyframeNums, k)
		}
		to.Keyframes = pts
	} else {
		to.Kind = "audio"
	}
	dur, err := si.Timeline.ToPresentation(si.MediaDur)
	if err != nil {
		return nil, err
	}
	to.Duration = dur
	return to, nil
}

// Build registers the four pipeline nodes on r. inputDir holds the
// generated representation files.
func Build(r *scheduler.Runner, p Params, inputDir string) error {
	segDur := p.SegmentSec
	if segDur <= 0 {
		segDur = 2
	}
	nodes := []*scheduler.Node{
		{Name: "scan", Version: 1, Run: func(c *scheduler.Ctx) (any, error) {
			paths, err := EnsureInputs(inputDir, p)
			if err != nil {
				return nil, err
			}
			out := &ScanOut{}
			for i, path := range paths {
				f, err := isobmff.Read(path)
				if err != nil {
					return nil, err
				}
				idx := isobmff.BuildIndexes(f, isobmff.DefaultLimits)
				out.Faults = append(out.Faults, idx.Faults...)
				rep := RepOut{Name: fmt.Sprintf("rep_%dk", p.BitratesKbps[i]), BitrateKbps: p.BitratesKbps[i]}
				for _, si := range idx.Tracks {
					to, err := trackOut(si, si.Track.IsVideo())
					if err != nil {
						return nil, err
					}
					if si.Track.IsVideo() {
						rep.Video = to
					} else if si.Track.IsAudio() {
						rep.Audio = to
					}
				}
				out.Reps = append(out.Reps, rep)
			}
			if b := out.Faults.Blocking(); len(b) > 0 {
				return nil, b[0]
			}
			return out, nil
		}},
		{Name: "normalize", Version: 1, Deps: []string{"scan"}, Run: func(c *scheduler.Ctx) (any, error) {
			var scan ScanOut
			if err := c.Dep("scan", &scan); err != nil {
				return nil, err
			}
			out := &NormOut{}
			var names []string
			var srcs []*isobmff.ColorInfo
			for _, rep := range scan.Reps {
				ad, fl := normalize.Audio(rep.Audio.Number, rep.Audio.Channels, rep.Audio.SampleRate)
				if fl != nil {
					return nil, fl
				}
				out.Audio = append(out.Audio, *ad)
				names = append(names, rep.Name)
				srcs = append(srcs, rep.Video.Color)
			}
			var target *isobmff.ColorInfo
			if p.ColorPolicy == string(normalize.PolicyUnifyTarget) {
				t := isobmff.ColorInfo{Primaries: 1, Transfer: 1, Matrix: 1}
				target = &t
			}
			cd, err := normalize.ResolveColor(names, srcs, normalize.ColorPolicy(p.ColorPolicy), target)
			if err != nil {
				return nil, err
			}
			out.Color = cd
			return out, nil
		}},
		{Name: "segment", Version: 1, Deps: []string{"scan", "normalize"}, Run: func(c *scheduler.Ctx) (any, error) {
			var scan ScanOut
			if err := c.Dep("scan", &scan); err != nil {
				return nil, err
			}
			var reps []manifest.Representation
			for _, ro := range scan.Reps {
				reps = append(reps, manifest.Representation{
					Name: ro.Name, BitrateKbps: ro.BitrateKbps,
					KeyframeNums: ro.Video.KeyframeNums, Keyframes: ro.Video.Keyframes,
					Duration: ro.Video.Duration,
				})
			}
			m, err := manifest.Align(reps, clock.Must(segDur, 1))
			if err != nil {
				return nil, err
			}
			return &SegOut{Manifest: m}, nil
		}},
		{Name: "checksum", Version: 1, Deps: []string{"scan", "normalize", "segment"}, Run: func(c *scheduler.Ctx) (any, error) {
			var scan ScanOut
			var norm NormOut
			var seg SegOut
			if err := c.Dep("scan", &scan); err != nil {
				return nil, err
			}
			if err := c.Dep("normalize", &norm); err != nil {
				return nil, err
			}
			if err := c.Dep("segment", &seg); err != nil {
				return nil, err
			}
			faults := append(fault.List{}, scan.Faults...)
			faults = append(faults, seg.Manifest.Faults...)
			bundle := struct {
				Faults   fault.List               `json:"faults,omitempty"`
				Audio    []normalize.AudioDesc    `json:"audio"`
				Color    *normalize.ColorDecision `json:"color"`
				Manifest *manifest.Manifest       `json:"manifest"`
			}{faults, norm.Audio, norm.Color, seg.Manifest}
			sum, err := archive.Checksum(bundle)
			if err != nil {
				return nil, err
			}
			return &FinalOut{
				Checksum: sum, Segments: len(seg.Manifest.Segments),
				Representations:   seg.Manifest.Representations,
				MaxBoundarySkewMs: seg.Manifest.MaxBoundarySkewMs,
				Faults:            faults,
			}, nil
		}},
	}
	for _, n := range nodes {
		if err := r.Add(n); err != nil {
			return err
		}
	}
	return nil
}

// SaveParams persists the run parameters beside the scheduler state.
func SaveParams(dir string, p Params) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "params.json"), raw, 0o644)
}

// LoadParams reads persisted run parameters.
func LoadParams(dir string) (Params, error) {
	var p Params
	raw, err := os.ReadFile(filepath.Join(dir, "params.json"))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(raw, &p)
}

// ParseBitrates parses "800k,1800k,3500k" into kbps values.
func ParseBitrates(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		k, ok := strings.CutSuffix(part, "k")
		if !ok {
			return nil, fmt.Errorf("invalid bitrate %q (want e.g. 800k)", part)
		}
		v, err := strconv.Atoi(k)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("invalid bitrate %q", part)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no bitrates given")
	}
	return out, nil
}
