package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediac/internal/clock"
	"mediac/internal/fanout"
	"mediac/internal/fixture"
	"mediac/internal/model"
	"mediac/internal/normalize"
	"mediac/internal/probe"
	"mediac/internal/recovery"
	"mediac/internal/segment"
	"mediac/internal/verify"
)

// config is the run configuration; its digest guards checkpoints.
type config struct {
	Reps    int                   `json:"reps"`
	Frames  int                   `json:"frames"`
	SegDurS int64                 `json:"segment_duration_s"`
	Audio   normalize.AudioTarget `json:"audio"`
	Color   normalize.ColorTarget `json:"color"`
}

func defaultConfig() config {
	return config{Reps: 3, Frames: 100, SegDurS: 2,
		Audio: normalize.AudioTarget{Channels: 2, SampleRate: 48000},
		Color: normalize.ColorTarget{Primaries: 1, Transfer: 1, Matrix: 1}}
}

func (c config) digest() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// summary is the terminal report of a pipeline run.
type summary struct {
	Segments           int                `json:"segments"`
	BoundaryMaxDeltaMs int64              `json:"boundary_max_delta_ms"`
	SHA256             string             `json:"sha256,omitempty"`
	Interrupted        bool               `json:"interrupted,omitempty"`
	ResumeMs           int64              `json:"resume_ms,omitempty"`
	Normalize          *normalize.Report  `json:"normalize,omitempty"`
	Damaged            []model.MediaError `json:"damaged,omitempty"`
}

type repState struct {
	id     string
	path   string
	srcSHA string
	res    *probe.Result
}

// ensureFixtures generates the self-contained sample set once.
func ensureFixtures(dir string, c config) *model.MediaError {
	fx := filepath.Join(dir, "fixtures")
	write := func(name string, spec fixture.Spec) *model.MediaError {
		p := filepath.Join(fx, name)
		if _, err := os.Stat(p); err == nil {
			return nil // keep fixtures stable across resume
		}
		if _, err := fixture.WriteFile(fx, name, spec); err != nil {
			return model.New(model.StageProbe, "fixture_failed", err.Error(), 1)
		}
		return nil
	}
	for i := 0; i < c.Reps; i++ {
		if merr := write(fixture.RepName(i), fixture.Standard(i+1, c.Frames)); merr != nil {
			return merr
		}
	}
	trunc := fixture.Standard(1, c.Frames)
	trunc.MoovTruncAt = fixture.MoovSize(trunc) - 50
	if merr := write("truncated_moov.mp4", trunc); merr != nil {
		return merr
	}
	corrupt := fixture.Standard(1, c.Frames)
	corrupt.MdatShortfall = 500
	if merr := write("corrupt_mdat.mp4", corrupt); merr != nil {
		return merr
	}
	nonmono := fixture.Standard(1, c.Frames)
	off := make([]int32, c.Frames)
	off[3] = -36000
	nonmono.Tracks[0].CttsOffsets = off
	if merr := write("nonmonotonic.mp4", nonmono); merr != nil {
		return merr
	}
	unknown := fixture.Standard(1, c.Frames)
	unknown.Tracks[0].Codec = "xyz1"
	return write("unknown_codec.mp4", unknown)
}

// probeReps probes every representation and runs normalization.
func probeReps(dir string, c config) ([]repState, *normalize.Report, *model.MediaError) {
	var reps []repState
	var audio []normalize.RepAudio
	var color []normalize.RepColor
	for i := 0; i < c.Reps; i++ {
		id := fmt.Sprintf("rep%d", i+1)
		p := filepath.Join(dir, "fixtures", fixture.RepName(i))
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, model.New(model.StageProbe, "read_failed", err.Error(), 1)
		}
		sum := sha256.Sum256(b)
		res, merr := probe.Probe(p)
		if merr != nil {
			merr.Representation = id
			return nil, nil, merr
		}
		reps = append(reps, repState{id: id, path: p,
			srcSHA: hex.EncodeToString(sum[:]), res: res})
		a := res.Track("audio")
		audio = append(audio, normalize.RepAudio{ID: id, Channels: a.Channels, SampleRate: a.SampleRate})
		var cm *normalize.ColorMeta
		if v := res.Track("video"); v.Color != nil {
			cm = &normalize.ColorMeta{Primaries: v.Color.Primaries, Transfer: v.Color.Transfer,
				Matrix: v.Color.Matrix, FullRange: v.Color.FullRange}
		}
		color = append(color, normalize.RepColor{ID: id, Meta: cm})
	}
	rep := normalize.NormalizeAudio(audio, c.Audio)
	cr := normalize.NormalizeColor(color, &c.Color)
	rep.Diffs = append(rep.Diffs, cr.Diffs...)
	rep.Unfixable = append(rep.Unfixable, cr.Unfixable...)
	if !rep.Fixable() {
		return nil, &rep, model.New(model.StageNormalize, "unfixable",
			"unfixable normalize differences, manual review required", 1)
	}
	return reps, &rep, nil
}

// extract returns the sealed segment payload for [start, end).
func extract(src []byte, res *probe.Result, start, end clock.Rational) []byte {
	var payload []byte
	for _, tr := range res.Tracks {
		for _, s := range tr.Samples {
			t := clock.New(s.DTS, tr.Timescale)
			if t.Cmp(start) >= 0 && t.Cmp(end) < 0 {
				payload = append(payload, src[s.Offset:s.Offset+s.Size]...)
			}
		}
	}
	return payload
}

// run executes the pipeline; interruptAfter > 0 simulates a crash after
// that many executed segment tasks. Incomplete runs return done=false.
func run(out string, c config, interruptAfter int) (*summary, bool, *model.MediaError) {
	if merr := ensureFixtures(out, c); merr != nil {
		return nil, false, merr
	}
	reps, normRep, merr := probeReps(out, c)
	if merr != nil {
		return nil, false, merr
	}
	probeRes := make([]*probe.Result, len(reps))
	for i, r := range reps {
		probeRes[i] = r.res
	}
	cuts, merr := segment.CommonCuts(probeRes, clock.New(c.SegDurS, 1))
	if merr != nil {
		return nil, false, merr
	}
	cfgSHA := c.digest()
	executed := 0
	for _, rep := range reps {
		ck, merr := recovery.Load(out, rep.id)
		if merr != nil {
			return nil, false, merr
		}
		if ck == nil {
			ck = &recovery.Checkpoint{RepID: rep.id, SourceSHA256: rep.srcSHA, ConfigSHA256: cfgSHA}
		}
		if merr := ck.Validate(rep.srcSHA, cfgSHA); merr != nil {
			return nil, false, merr
		}
		src, err := os.ReadFile(rep.path)
		if err != nil {
			return nil, false, model.New(model.StageFanout, "read_failed", err.Error(), 1)
		}
		var tasks []fanout.Task
		for i := 0; i+1 < len(cuts); i++ {
			if ck.Has(i) {
				continue // completed segments are never rewritten
			}
			tasks = append(tasks, fanout.Task{RepID: rep.id, Index: i, Start: cuts[i], End: cuts[i+1]})
		}
		var afterErr *model.MediaError
		w := fanout.Worker{After: func(r fanout.Result) {
			if afterErr != nil {
				return
			}
			e, merr := segment.Entry(r.Task.Index, r.Task.Start, r.Task.End, r.SHA256, r.Bytes, r.RTPTimestamp)
			if merr == nil {
				ck.Completed = append(ck.Completed, e)
				merr = recovery.Save(out, ck)
			}
			afterErr = merr
		}}
		_, merr = w.Run(tasks, func(t fanout.Task) (fanout.Result, *model.MediaError) {
			if interruptAfter > 0 && executed >= interruptAfter {
				return fanout.Result{}, errInterrupted
			}
			rtpTS, merr := fanout.RTPBase(t.Start)
			if merr != nil {
				return fanout.Result{}, merr
			}
			sealed, merr := fanout.Seal(t.RepID, t.Index, rtpTS, extract(src, rep.res, t.Start, t.End))
			if merr != nil {
				return fanout.Result{}, merr
			}
			p := verify.SegmentPath(out, t.RepID, t.Index)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return fanout.Result{}, model.New(model.StageFanout, "write_failed", err.Error(), 1)
			}
			if err := os.WriteFile(p, sealed, 0o644); err != nil {
				return fanout.Result{}, model.New(model.StageFanout, "write_failed", err.Error(), 1)
			}
			sum := sha256.Sum256(sealed)
			executed++
			return fanout.Result{Task: t, SHA256: hex.EncodeToString(sum[:]),
				Bytes: int64(len(sealed)), RTPTimestamp: rtpTS}, nil
		})
		if merr != nil && merr.Code == "interrupted" {
			return &summary{Interrupted: true, Normalize: normRep}, false, nil
		}
		if merr != nil {
			return nil, false, merr
		}
		if afterErr != nil {
			return nil, false, afterErr
		}
	}
	// Fan-in: assemble the manifest from the per-representation
	// checkpoints so a resumed run yields identical bytes.
	m := &segment.Manifest{Version: 1, ConfigSHA256: cfgSHA, SegmentDurationMs: c.SegDurS * 1000}
	for _, rep := range reps {
		ck, _ := recovery.Load(out, rep.id)
		m.Representations = append(m.Representations, segment.RepManifest{
			ID: rep.id, Source: filepath.Base(rep.path), SourceSHA256: rep.srcSHA,
			Segments: ck.Completed})
	}
	delta, merr := verify.FanIn(m, 40)
	if merr != nil {
		return nil, false, merr
	}
	sha, merr := segment.Write(out, m)
	if merr != nil {
		return nil, false, merr
	}
	n := 0
	for _, r := range m.Representations {
		n += len(r.Segments)
	}
	return &summary{Segments: n, BoundaryMaxDeltaMs: delta, SHA256: sha, Normalize: normRep}, true, nil
}

var errInterrupted = &model.MediaError{Stage: model.StageFanout, Code: "interrupted",
	Message: "simulated interrupt", SegmentIndex: -1, Exit: 1}
