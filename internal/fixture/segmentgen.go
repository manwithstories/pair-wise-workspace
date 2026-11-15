// Package fixture synthesizes transcoding-cluster segment metadata for a
// 7-rung ladder (5 video + 2 audio) entirely locally. It also injects
// anomaly samples (missing segment, time regression, zero duration,
// out-of-order periods) for verify-command drills.
package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SegmentMeta is one segment as reported by the transcoding cluster.
type SegmentMeta struct {
	N int   `json:"n" yaml:"n"`
	T int64 `json:"t" yaml:"t"` // ticks
	D int64 `json:"d" yaml:"d"` // ticks
}

// PeriodMeta groups the segments of one period for one representation.
type PeriodMeta struct {
	Index    int           `json:"index" yaml:"index"`
	StartMS  int64         `json:"start_ms" yaml:"start_ms"`
	Segments []SegmentMeta `json:"segments" yaml:"segments"`
}

// RepMeta is the full metadata of one ladder rung.
type RepMeta struct {
	ID        string       `json:"id" yaml:"id"`
	Kind      string       `json:"kind" yaml:"kind"` // video | audio
	Width     int          `json:"width,omitempty" yaml:"width,omitempty"`
	Height    int          `json:"height,omitempty" yaml:"height,omitempty"`
	FrameRate string       `json:"frame_rate,omitempty" yaml:"frame_rate,omitempty"`
	Codecs    string       `json:"codecs" yaml:"codecs"`
	Bandwidth int64        `json:"bandwidth" yaml:"bandwidth"`
	PTOTicks  int64        `json:"pto_ticks" yaml:"pto_ticks"`
	Periods   []PeriodMeta `json:"periods" yaml:"periods"`
}

// File is the on-disk fixture document.
type File struct {
	Timescale  uint64    `json:"timescale" yaml:"timescale"`
	GOPMS      int64     `json:"gop_ms" yaml:"gop_ms"`
	DurationMS int64     `json:"duration_ms" yaml:"duration_ms"`
	Reps       []RepMeta `json:"reps" yaml:"reps"`
}

// Options controls synthesis.
type Options struct {
	Periods    int
	GOPMS      int64
	DurationMS int64
	Timescale  uint64
	Anomaly    string // "", "missing", "regression", "zerodur", "periodorder", "offset"
}

type rung struct {
	id         string
	kind       string
	w, h       int
	fr, codecs string
	bw         int64
}

var ladderRungs = []rung{
	{"v240p", "video", 426, 240, "30000/1001", "avc1.42E01E", 300000},
	{"v360p", "video", 640, 360, "30000/1001", "avc1.4D401F", 800000},
	{"v480p", "video", 854, 480, "30000/1001", "avc1.4D401F", 1200000},
	{"v720p", "video", 1280, 720, "30000/1001", "avc1.640028", 2500000},
	{"v1080p", "video", 1920, 1080, "30000/1001", "avc1.640032", 5000000},
	{"a64k", "audio", 0, 0, "", "mp4a.40.2", 64000},
	{"a128k", "audio", 0, 0, "", "mp4a.40.2", 128000},
}

// Generate builds a fixture where every rung shares the same GOP grid so
// cross-rung alignment is exact. The first segment of period 0 carries
// pre-roll (t = pto), the last segment of each period is shorter than one
// GOP, and period boundaries cut mid-GOP.
func Generate(o Options) (*File, error) {
	if o.Periods < 1 || o.GOPMS <= 0 || o.DurationMS <= 0 || o.Timescale == 0 {
		return nil, fmt.Errorf("invalid fixture options: %+v", o)
	}
	gopTicks := o.GOPMS * int64(o.Timescale) / 1000
	if gopTicks*1000 != o.GOPMS*int64(o.Timescale) {
		return nil, fmt.Errorf("gop %dms not exact at timescale %d", o.GOPMS, o.Timescale)
	}
	// Pre-roll: exactly one 30000/1001 frame = 3003 ticks at 90000.
	preroll := int64(o.Timescale) * 1001 / 30000
	perPeriodMS := o.DurationMS / int64(o.Periods)
	f := &File{Timescale: o.Timescale, GOPMS: o.GOPMS, DurationMS: o.DurationMS}
	for _, r := range ladderRungs {
		rep := RepMeta{ID: r.id, Kind: r.kind, Width: r.w, Height: r.h,
			FrameRate: r.fr, Codecs: r.codecs, Bandwidth: r.bw, PTOTicks: preroll}
		for p := 0; p < o.Periods; p++ {
			pm := PeriodMeta{Index: p, StartMS: int64(p) * perPeriodMS}
			remain := perPeriodMS * int64(o.Timescale) / 1000
			t := int64(0)
			if p == 0 {
				t = preroll // first segment carries pre-roll
			}
			n := 1
			for remain > 0 {
				d := gopTicks
				if d > remain {
					d = remain // tail shorter than one GOP
				}
				pm.Segments = append(pm.Segments, SegmentMeta{N: n, T: t, D: d})
				t += d
				remain -= d
				n++
			}
			rep.Periods = append(rep.Periods, pm)
		}
		f.Reps = append(f.Reps, rep)
	}
	injectAnomaly(f, o.Anomaly)
	return f, nil
}

func injectAnomaly(f *File, kind string) {
	if kind == "" || len(f.Reps) == 0 {
		return
	}
	segs := f.Reps[0].Periods[0].Segments
	switch kind {
	case "missing": // drop a middle segment, leaving a t-gap
		mid := len(segs) / 2
		f.Reps[0].Periods[0].Segments = append(segs[:mid], segs[mid+1:]...)
	case "regression": // rewind one segment's t
		segs[len(segs)/2].T -= 2 * segs[len(segs)/2].D
	case "zerodur":
		segs[len(segs)/2].D = 0
	case "periodorder": // swap period starts so they run out of order
		if len(f.Reps[0].Periods) > 1 {
			ps := f.Reps[0].Periods
			ps[0].StartMS, ps[1].StartMS = ps[1].StartMS, ps[0].StartMS
		}
	case "offset": // shift one audio rung by 5ms worth of ticks
		shift := int64(f.Timescale) * 5 / 1000
		for pi := range f.Reps[5].Periods {
			segs := f.Reps[5].Periods[pi].Segments
			for i := range segs {
				segs[i].T += shift
			}
		}
	}
}

// Write stores the fixture as JSON or YAML based on the file extension.
func Write(f *File, path string) error {
	var data []byte
	var err error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		data, err = yaml.Marshal(f)
	default:
		data, err = json.MarshalIndent(f, "", "  ")
	}
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// Read loads a JSON or YAML fixture.
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &File{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		err = yaml.Unmarshal(data, f)
	default:
		err = json.Unmarshal(data, f)
	}
	if err != nil {
		return nil, fmt.Errorf("parse fixture %s: %w", path, err)
	}
	return f, nil
}
