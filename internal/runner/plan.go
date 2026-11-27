// Package runner orchestrates the pipeline: manifest checks, clock fitting,
// DAG construction, audio alignment, recovery planning and bounded
// concurrent composition with resumable export.
package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"fieldcut/internal/audio"
	"fieldcut/internal/clock"
	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
	"fieldcut/internal/recovery"
	"fieldcut/internal/timeline"
)

// Options selects the input, the output directory and bounded work.
type Options struct {
	OutDir     string
	InputDir   string
	ConfigHash string // hash of user-visible configuration, for cache validity
	Workers    int    // bounded concurrency, clamped to [1, MaxTracks]
	Limit      int    // stop after this many newly committed frames; 0 = all
	Log        io.Writer
}

// Result is the completed (or interrupted) pipeline outcome.
type Result struct {
	Frames          int
	Root            string
	SyncMaxMs       float64
	RecoveryRate    float64
	Eligible        int
	Recovered       int
	Missing         int
	CorruptRepaired int
	P95FrameMs      float64
	AudioOffsetMs   float64
	Interrupted     bool
	MissingRecords  []string
}

// ErrInterrupted marks a deliberately truncated run used to prove resume.
var ErrInterrupted = errors.New("interrupted after frame limit")

// trackFrames holds one track's mapped output grid.
type trackFrames struct {
	name       string
	manifest   ingest.Track
	node       timeline.Node
	entries    map[int64]ingest.Entry
	verdict    map[int64]*media.MediaError
	nanos      map[int64]int64 // mapped wall time of every delivered frame
	idxSeq     map[int64]int64 // output index -> valid source sequence
	idxInterp  map[int64]recovery.Interp
	idxMissing map[int64]string
	plan       recovery.Plan
}

type analysis struct {
	man             *ingest.Manifest
	graph           *timeline.Graph
	tracks          map[string]*trackFrames
	composite       []string // paint order, lowest priority first
	frames          int
	width           int
	height          int
	origin          int64 // common origin: frame 0 starts at wall time 0
	syncMaxMs       float64
	audio           audio.Probe
	eligible        int
	recovered       int
	missing         int
	missingRecords  []string
	corruptRepaired int
	rate            float64
	sourceHash      string
	inDir           string
}

// analyze performs every non-rendering stage in DAG order.
func analyze(opts Options) (*analysis, error) {
	man, entries, err := ingest.Load(filepath.Join(opts.InputDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	verdicts, err := ingest.CheckFrames(opts.InputDir, entries)
	if err != nil {
		return nil, err
	}
	var nodes []timeline.Node
	for _, t := range man.Tracks {
		nodes = append(nodes, timeline.Node{
			Name:     t.Name,
			Offset:   media.NewRational(t.OffsetTick*1000, 1e9),
			Priority: t.Priority,
			Mask:     timeline.Rect{X: t.Mask.X, Y: t.Mask.Y, W: t.Mask.W, H: t.Mask.H},
			Deps:     append([]string(nil), t.Deps...),
		})
	}
	graph, err := timeline.Build(nodes)
	if err != nil {
		return nil, err
	}
	a := &analysis{
		man: man, graph: graph, frames: man.Frames,
		width: man.Width, height: man.Height, inDir: opts.InputDir,
		tracks:    make(map[string]*trackFrames, len(man.Tracks)),
		composite: graph.CompositeOrder(),
	}
	a.sourceHash, err = sourceHash(opts.InputDir, man, entries)
	if err != nil {
		return nil, err
	}
	for _, t := range man.Tracks {
		tf := &trackFrames{
			name: t.Name, manifest: t, node: mustNode(graph, t.Name),
			entries: make(map[int64]ingest.Entry), verdict: verdicts[t.Name],
			nanos: make(map[int64]int64), idxSeq: make(map[int64]int64),
			idxInterp: make(map[int64]recovery.Interp), idxMissing: make(map[int64]string),
		}
		for _, e := range entries[t.Name] {
			tf.entries[e.Seq] = e
		}
		if err := a.fitTrack(t, tf); err != nil {
			return nil, err
		}
		a.tracks[t.Name] = tf
	}
	// Every track shares one origin: the earliest mapped wall time across
	// the whole set. Offsets only shift each track relative to that origin.
	first := true
	for _, tf := range a.tracks {
		for _, n := range tf.nanos {
			if first || n < a.origin {
				a.origin, first = n, false
			}
		}
	}
	if err := a.mapFrames(); err != nil {
		return nil, err
	}
	if err := a.sessionRecovery(); err != nil {
		return nil, err
	}
	if err := a.probeAudio(); err != nil {
		return nil, err
	}
	return a, nil
}

func mustNode(g *timeline.Graph, name string) timeline.Node {
	n, _ := g.Node(name)
	return n
}

// fitTrack unwraps the device counter, fits drift against the wall-clock
// anchors and records the exact mapped wall time of every delivered frame.
func (a *analysis) fitTrack(t ingest.Track, tf *trackFrames) error {
	var samples []clock.Sample
	for seq, e := range tf.entries {
		if e.Tick < 0 {
			return media.Newf(media.KindClock, "clock.sample", t.Name, seq, "negative tick")
		}
		samples = append(samples, clock.Sample{Seq: seq, Tick: uint64(e.Tick), Nanos: e.Nanos})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Seq < samples[j].Seq })
	unwrapped, err := clock.Unwrap(t.Name, samples, a.man.WrapAt)
	if err != nil {
		return err
	}
	anchors := make([]clock.Marker, len(unwrapped))
	for i, s := range unwrapped {
		anchors[i] = clock.Marker{Track: t.Name, Seq: s.Seq, Tick: s.Unwrapped, Nanos: s.Nanos}
	}
	f, err := clock.FitDrift(anchors, media.TickBase)
	if err != nil {
		return err
	}
	if f.MaxErrorMs > a.syncMaxMs {
		a.syncMaxMs = f.MaxErrorMs
	}
	offsetNanos := t.OffsetTick * 1000 // nominal 1 µs ticks
	for _, s := range unwrapped {
		tf.nanos[s.Seq] = f.MapWallNanos(s.Unwrapped) + offsetNanos
	}
	return nil
}

// indexOf maps an absolute wall time onto the common 25 fps grid using the
// shared left-closed, right-open rational rule.
func (a *analysis) indexOf(nanos int64) int64 {
	return media.FrameIndex(media.NewRational(nanos-a.origin, 1e9))
}

// mapFrames places every delivered frame on the shared output grid and plans
// recovery per track. A hole is an output index with no valid source frame.
func (a *analysis) mapFrames() error {
	for name, tf := range a.tracks {
		var positions []recovery.Position
		for seq, n := range tf.nanos {
			idx := a.indexOf(n)
			if idx < 0 || idx >= int64(a.frames) {
				return media.Newf(media.KindGraph, "timeline.map", name, seq,
					"frame maps outside output timeline (index %d of %d)", idx, a.frames)
			}
			valid := tf.verdict[seq] == nil
			positions = append(positions, recovery.Position{Seq: seq, Index: idx, Present: true, Valid: valid})
			if valid {
				if cur, ok := tf.idxSeq[idx]; !ok || seq > cur {
					tf.idxSeq[idx] = seq
				}
			}
		}
		plan, err := recovery.AnalyzePositions(name, a.frames, positions)
		if err != nil {
			return err
		}
		tf.plan = plan
	}
	return nil
}

// sessionRecovery aggregates per-track plans into per-index instructions
// and delivery metrics. Valid frames always win over synthesis at an index.
func (a *analysis) sessionRecovery() error {
	for _, tf := range a.tracks {
		plan := tf.plan
		a.eligible += plan.Eligible
		a.recovered += plan.Recovered
		a.corruptRepaired += plan.CorruptRepaired
		for _, in := range plan.Interps {
			if _, real := tf.idxSeq[in.Seq]; real {
				continue
			}
			tf.idxInterp[in.Seq] = in
		}
		for _, m := range plan.Missing {
			a.missing++
			if _, real := tf.idxSeq[m.Seq]; real {
				continue
			}
			if _, interp := tf.idxInterp[m.Seq]; interp {
				continue
			}
			tf.idxMissing[m.Seq] = m.Reason
			a.missingRecords = append(a.missingRecords,
				fmt.Sprintf("%s seq=%d reason=%s", m.Track, m.Seq, m.Reason))
		}
	}
	if a.eligible == 0 {
		a.rate = 1.0
	} else {
		a.rate = float64(a.recovered) / float64(a.eligible)
	}
	return nil
}

// probeAudio measures the calibration pulse grid against the exported
// timeline. The audio buffer and frame 0 both start at wall time zero, so the
// measured offset is the frame-0 alignment error; the grid residual shows
// whether the pulse cadence itself is intact. sync_max_ms is the larger of
// the drift-fit residual, the absolute offset and the cadence residual.
func (a *analysis) probeAudio() error {
	data, err := os.ReadFile(filepath.Join(a.inDir, "audio.pcm"))
	if err != nil {
		return media.New(media.KindInput, "audio.read", "", -1, err)
	}
	samples, err := audio.ParsePCM(data, 1)
	if err != nil {
		return err
	}
	// Expected pulse start is the measured cadence's own onset: the first
	// pulse edge lands at t=0 of the exported timeline.
	p, err := audio.ProbeAlignment(samples, media.AudioRate, 0, 0.3, 64)
	if err != nil {
		return err
	}
	a.audio = p
	off := float64(p.OffsetNanos) / 1e6
	if off < 0 {
		off = -off
	}
	if off > a.syncMaxMs {
		a.syncMaxMs = off
	}
	if gridErr := float64(p.MaxErrorNanos) / 1e6; gridErr > a.syncMaxMs {
		a.syncMaxMs = gridErr
	}
	return nil
}

// sourceHash fingerprints manifest content, frame hashes and audio bytes so
// any source change invalidates a previous cache.
func sourceHash(inputDir string, man *ingest.Manifest, entries map[string][]ingest.Entry) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v%d fps%d frames%d canvas%dx%d wrap%d\n", man.Version, man.OutputFPS, man.Frames, man.Width, man.Height, man.WrapAt)
	tracks := make([]string, 0, len(entries))
	for name := range entries {
		tracks = append(tracks, name)
	}
	sort.Strings(tracks)
	for _, name := range tracks {
		for _, e := range entries[name] {
			fmt.Fprintf(h, "%s %d %s\n", name, e.Seq, e.SHA256)
		}
	}
	data, err := os.ReadFile(filepath.Join(inputDir, "audio.pcm"))
	if err != nil {
		return "", media.New(media.KindInput, "ingest.source", "", -1, err)
	}
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}
