package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"bcast/demux"
	"bcast/fingerprint"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/output"
	"bcast/segments"
	"bcast/silence"
)

// delivery is one generated input variant and everything derived from it.
// Fields are written by DAG nodes; a mutex guards them because sibling
// deliveries run concurrently (each delivery's own chain is sequential).
type delivery struct {
	name string
	data []byte

	mu       sync.Mutex
	parsed   bool
	parseErr error
	f        *demux.File
	indexOK  bool
	segs     []segments.Segment
	silence  *silence.Report
	hashes   []string
	out      []output.Report
	err      error
}

func (d *delivery) reader() *bytes.Reader { return bytes.NewReader(d.data) }

// Run executes the batch: generate material, then demux, index, cut, detect
// silence, fingerprint and remux each delivery. A delivery whose input is
// unusable is quarantined — its downstream nodes are reported blocked — while
// the rest of the batch continues. The returned Summary carries the metrics
// the acceptance command prints.
func Run(ctx context.Context, p Params, cp *Checkpoint) (*Summary, error) {
	if p.OutDir == "" {
		return nil, fmt.Errorf("pipeline: no output directory")
	}
	if err := os.MkdirAll(p.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("pipeline: create output dir: %w", err)
	}
	assets, err := fixture.Generate(fixture.Default(p.Seed, p.Segments))
	if err != nil {
		return nil, fmt.Errorf("pipeline: generate: %w", err)
	}
	deliveries := buildDeliveries(p, assets)
	primary := deliveries[0]
	sig := p.Signature()

	dag := New(cp)
	for i := range deliveries {
		d := deliveries[i]
		key := func(stage string) string { return fingerprint.Key(sig, stage+":"+d.name, i) }
		addDeliveryNodes(dag, d, key, p)
	}
	dag.Execute(ctx, p.Workers)
	sum := summarize(deliveries, dag, p)
	if err := writeDamagedManifest(p.OutDir, sum); err != nil {
		return nil, err
	}
	cp.SetMeta("params", sig)
	cp.SetMeta("outdir", p.OutDir)
	cp.SetMeta("primary", primary.name)
	cp.SetMeta("summary", sum.Encode())
	if err := cp.Save(); err != nil {
		return nil, fmt.Errorf("pipeline: save checkpoint: %w", err)
	}
	return sum, nil
}

// writeDamagedManifest records every unsalvageable byte range next to the
// outputs, so the damaged areas are auditable without re-parsing the inputs.
func writeDamagedManifest(outDir string, sum *Summary) error {
	path := filepath.Join(outDir, "damaged-ranges.txt")
	tmp, err := os.CreateTemp(outDir, ".bcast-damaged-*")
	if err != nil {
		return fmt.Errorf("pipeline: damaged manifest: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(sum.DamagedManifest()); err != nil {
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
	return os.Rename(name, path)
}

// buildDeliveries always starts with the clean program (the primary
// deliverable whose metrics are reported), adds one variant per requested
// damage to exercise fault handling, and appends a quarantined corrupt input
// so failure propagation and the first blocking dependency are exercised on
// every run.
func buildDeliveries(p Params, a *fixture.Assets) []*delivery {
	out := []*delivery{{name: "clean", data: a.Clean}}
	for _, dmg := range p.Damage {
		switch dmg {
		case DamageTruncate:
			out = append(out, &delivery{name: "truncate", data: a.Truncated})
		case DamageBrokenBox:
			out = append(out, &delivery{name: "broken-box", data: a.BrokenBox})
		case DamageMissingIndex:
			out = append(out, &delivery{name: "missing-index", data: a.MissingIndex})
		}
	}
	out = append(out, &delivery{name: "corrupt", data: []byte("this delivery has no moov box and cannot be recovered")})
	return out
}

// addDeliveryNodes wires the per-delivery chain. Node IDs are stable across
// runs so a checkpoint from a previous run can resume them.
func addDeliveryNodes(dag *DAG, d *delivery, key func(string) string, p Params) {
	id := func(stage string) string { return stage + ":" + d.name }
	must(dag.Add(Node{ID: id("parse"), Key: key("parse"), Run: func(context.Context) (string, error) {
		f, err := demux.Parse(d.reader(), int64(len(d.data)), d.name)
		d.mu.Lock()
		d.f, d.parsed, d.err = f, true, nil
		d.mu.Unlock()
		if err != nil && media.ClassOf(err) == media.ClassFatal {
			return "", err
		}
		// Recoverable damage still yields a usable prefix; the node is marked
		// recovered by its error class rather than failing the chain.
		return fmt.Sprintf("samples=%d damaged=%d", len(f.Samples), len(f.Damaged)), err
	}}))
	must(dag.Add(Node{ID: id("index"), Deps: []string{id("parse")}, Key: key("index"), Run: func(context.Context) (string, error) {
		f, err := d.ensureParsed()
		if err != nil {
			return "", err
		}
		if err := d.buildIndex(f); err != nil {
			return "", err
		}
		total, keys, src := demux.KeyIndexStats(f)
		d.mu.Lock()
		d.indexOK = true
		d.mu.Unlock()
		return fmt.Sprintf("total=%d keys=%d source=%s", total, keys, src), nil
	}}))
	must(dag.Add(Node{ID: id("cut"), Deps: []string{id("index")}, Key: key("cut"), Run: func(context.Context) (string, error) {
		f, err := d.ensureIndexed()
		if err != nil {
			return "", err
		}
		segs, err := segments.Cut(&f.File, p.TargetDur)
		if err != nil {
			return "", err
		}
		d.mu.Lock()
		d.segs = segs
		d.mu.Unlock()
		return fmt.Sprintf("segments=%d", len(segs)), nil
	}}))
	must(dag.Add(Node{ID: id("silence"), Deps: []string{id("cut")}, Key: key("silence"), Run: func(context.Context) (string, error) {
		f, err := d.ensureParsed()
		if err != nil {
			return "", err
		}
		rep := silence.Detect(&f.File, d.reader(), p.WindowSize, p.Threshold)
		d.mu.Lock()
		d.silence = rep
		d.mu.Unlock()
		return fmt.Sprintf("windows=%d silent=%d unknown=%d status=%s",
			len(rep.Windows), rep.SilentWindows, rep.UnknownWindows, rep.Status), nil
	}}))
	must(dag.Add(Node{ID: id("fingerprint"), Deps: []string{id("cut")}, Key: key("fingerprint"), Run: func(context.Context) (string, error) {
		segs, err := d.ensureSegs(p.TargetDur)
		if err != nil {
			return "", err
		}
		hs := make([]string, len(segs))
		for i, s := range segs {
			h, err := fingerprint.Hash(d.reader(), s)
			if err != nil {
				return "", err
			}
			hs[i] = h
		}
		d.mu.Lock()
		d.hashes = hs
		d.mu.Unlock()
		return fmt.Sprintf("hashes=%d", len(hs)), nil
	}}))
	must(dag.Add(Node{ID: id("remux"), Deps: []string{id("silence"), id("fingerprint")}, Key: key("remux"), Run: func(context.Context) (string, error) {
		f, err := d.ensureParsed()
		if err != nil {
			return "", err
		}
		segs, err := d.ensureSegs(p.TargetDur)
		if err != nil {
			return "", err
		}
		dir := filepath.Join(p.OutDir, d.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		var reps []output.Report
		for _, seg := range segs {
			path := filepath.Join(dir, fmt.Sprintf("seg-%03d.mp4", seg.Index))
			rep, err := output.WriteFile(path, d.reader(), remuxTracks(f, seg))
			if err != nil {
				return "", err
			}
			reps = append(reps, rep)
		}
		d.mu.Lock()
		d.out = reps
		d.mu.Unlock()
		return fmt.Sprintf("files=%d", len(reps)), nil
	}}))
}

// remuxTracks converts a segment into output track descriptions, copying the
// source stsd and composition offsets and keeping absolute position.
func remuxTracks(f *demux.File, seg segments.Segment) []output.TrackMux {
	var out []output.TrackMux
	for _, tr := range f.Tracks {
		tm := output.TrackMux{ID: tr.ID, Kind: tr.Kind, Timescale: tr.Timescale, CodecEntry: tr.CodecEntry}
		var samples []media.Sample
		if tr.Kind == media.TrackAudio {
			samples = seg.Audio
			tm.StartOffset = seg.Start.ToTimescale(tr.Timescale).Value
		} else {
			samples = seg.Video
			tm.StartOffset = seg.Start.ToTimescale(tr.Timescale).Value
		}
		for _, s := range samples {
			tm.Samples = append(tm.Samples, output.SampleRef{
				Offset: s.Offset, Size: s.Size, Duration: s.Duration,
				Keyframe: s.Keyframe, CTSOffset: s.CTS.Value - s.DTS.Value,
			})
		}
		out = append(out, tm)
	}
	return out
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// ensureParsed lazily re-materialises the demuxed file. On resume a node may
// be reused from the checkpoint while a later node still runs, so the parse
// result has to be available even though the parse node did not execute.
// ensureIndexed is ensureParsed plus a rebuilt keyframe index, so a resumed
// run whose index node was reused still has keyframes when later nodes
// re-materialise the file.
func (d *delivery) ensureIndexed() (*demux.File, error) {
	f, err := d.ensureParsed()
	if err != nil {
		return nil, err
	}
	if err := d.buildIndex(f); err != nil {
		return nil, err
	}
	return f, nil
}

// buildIndex rebuilds the keyframe index once per delivery. It is idempotent:
// a container with stss is left untouched.
func (d *delivery) buildIndex(f *demux.File) error {
	d.mu.Lock()
	done := d.indexOK
	d.mu.Unlock()
	if done {
		return nil
	}
	if err := demux.BuildKeyIndex(f, d.reader()); err != nil {
		return err
	}
	d.mu.Lock()
	d.indexOK = true
	d.mu.Unlock()
	return nil
}

func (d *delivery) ensureParsed() (*demux.File, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.parsed {
		f, err := demux.Parse(d.reader(), int64(len(d.data)), d.name)
		d.f, d.parsed = f, true
		if err != nil && media.ClassOf(err) == media.ClassFatal {
			d.parseErr = err
		}
	}
	if d.f == nil {
		if d.parseErr != nil {
			return nil, d.parseErr
		}
		return nil, fmt.Errorf("pipeline: %s parse produced no file", d.name)
	}
	return d.f, nil
}

func (d *delivery) ensureSegs(target int64) ([]segments.Segment, error) {
	d.mu.Lock()
	segs := d.segs
	d.mu.Unlock()
	if segs != nil {
		return segs, nil
	}
	f, err := d.ensureIndexed()
	if err != nil {
		return nil, err
	}
	segs, err = segments.Cut(&f.File, target)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.segs = segs
	d.mu.Unlock()
	return segs, nil
}
