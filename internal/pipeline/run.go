// Package pipeline orchestrates chunked batch processing: RTP ingest,
// timeline merge, review-frame rendering and checkpointed indexing.
// Chunks are processed as they arrive (no fixed-size buffering of the
// whole stream), every committed chunk is checkpointed atomically, and
// a failed chunk never advances the checkpoint.
package pipeline

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediasync/internal/defect"
	"mediasync/internal/index"
	"mediasync/internal/ingest"
	"mediasync/internal/render"
	"mediasync/internal/subtitle"
	"mediasync/internal/timeline"
)

const (
	outputName  = "output.frames"
	markerEvery = 97
	dupEvery    = 211
	swapEvery   = 503
	ssrc        = 0x00015539
)

// Config parameterizes one pipeline run.
type Config struct {
	Seed           int64
	Packets        int
	Chunk          int
	LossBurst      int
	Bandwidth      []float64 // simulated pacing profile
	Corrupt        string    // "", "subtitle", "index" or "output"
	InterruptAfter int       // stop after N committed chunks; 0 = never
	Resume         bool      // resume from the last valid checkpoint
	Dir            string    // local output directory
}

// Result carries the acceptance data of one run.
type Result struct {
	BadOffsetBytes      int64               `json:"bad_offset_bytes"`
	LastValidCheckpoint int                 `json:"last_valid_checkpoint"`
	ResumeMs            int64               `json:"resume_ms"`
	MissingPackets      int                 `json:"missing_packets"`
	FrameCount          int                 `json:"frame_count"`
	FrameSHA256         string              `json:"frame_sha256"`
	OutputSHA256        string              `json:"output_sha256"`
	TrackSHA256         string              `json:"track_sha256"`
	Bandwidth           []float64           `json:"bandwidth"`
	Defects             []defect.Defect     `json:"defects"`
	Decisions           []timeline.Decision `json:"decisions"`
	ExitCode            int                 `json:"-"`
}

// Run executes the pipeline and returns acceptance data. ExitCode is 0
// on success, 2 when corrupt input was handled with defect records, 1
// when the index is unrecoverable.
func Run(cfg Config) (Result, error) {
	res := Result{BadOffsetBytes: -1, LastValidCheckpoint: -1, Bandwidth: cfg.Bandwidth}
	if cfg.Packets <= 0 || cfg.Chunk <= 0 {
		return res, fmt.Errorf("pipeline: packets and chunk must be positive")
	}
	if err := os.RemoveAll(cfg.Dir); err != nil {
		return res, err
	}
	store, err := index.Open(cfg.Dir)
	if err != nil {
		return res, err
	}
	outPath := filepath.Join(cfg.Dir, outputName)

	// Subtitle source, optionally corrupted at a known byte offset.
	src, badOff := generateSRT(cfg.Packets/4, cfg.Seed, cfg.Corrupt == "subtitle")
	cues, perr := subtitle.ParseSRT(src)
	var parseDefects []defect.Defect
	if perr != nil {
		pe, ok := perr.(*subtitle.ParseError)
		if !ok {
			return res, perr
		}
		res.BadOffsetBytes = pe.Offset
		if cfg.Corrupt == "subtitle" && abs(pe.Offset-badOff) > 1 {
			return res, fmt.Errorf("pipeline: corruption at %d located at %d", badOff, pe.Offset)
		}
		parseDefects = append(parseDefects, defect.New("subtitle", -1, pe.Offset, defect.SubtitleParse, pe.Msg))
		res.ExitCode = 2
	}

	datagrams, err := buildDatagrams(cues, cfg.Packets)
	if err != nil {
		return res, err
	}
	chunks := chunkify(impair(datagrams, cfg.LossBurst, cfg.Seed), cfg.Chunk)

	r := newRunner(cfg, chunks)
	r.out, err = os.Create(outPath)
	if err != nil {
		return res, err
	}
	stop := len(chunks)
	interrupted := cfg.InterruptAfter > 0 && cfg.InterruptAfter < stop
	if interrupted {
		stop = cfg.InterruptAfter
	}
	committed := 0
	for k := 0; k < stop; k++ {
		if err := r.process(k, true, store); err != nil {
			r.out.Close()
			return res, err
		}
		committed++
	}
	r.out.Close()

	corrupted := false
	if interrupted || cfg.Resume {
		switch cfg.Corrupt {
		case "index":
			if err := corruptLastCheckpoint(store); err != nil {
				return res, err
			}
			corrupted = true
		case "output":
			info, err := os.Stat(outPath)
			if err != nil {
				return res, err
			}
			if err := os.Truncate(outPath, info.Size()-100); err != nil {
				return res, err
			}
			corrupted = true
		}
	}

	final := r
	if (interrupted || corrupted) && cfg.Resume {
		final, err = resume(cfg, chunks, store, outPath, r.inPref, committed, &res)
		if err != nil || final == nil {
			res.Defects = parseDefects
			return res, err
		}
	} else {
		res.LastValidCheckpoint = stop - 1
	}

	res.MissingPackets = final.loss.Missing()
	res.FrameCount = final.gen.Count
	res.FrameSHA256 = final.gen.Summary()
	res.TrackSHA256 = final.merger.Summary()
	oh, _, err := index.HashPrefix(outPath, int64(1)<<62)
	if err != nil {
		return res, err
	}
	res.OutputSHA256 = oh
	res.Defects = append(parseDefects, final.defects...)
	res.Defects = append(res.Defects, final.reasm.Defects...)
	res.Defects = append(res.Defects, final.merger.Defects...)
	res.Decisions = final.merger.Decisions
	return res, nil
}

// resume recovers the last valid checkpoint, truncates the output to
// its cursor, fast-forwards state and processes the remaining chunks.
func resume(cfg Config, chunks [][][]byte, store *index.Store, outPath string,
	inPref []int64, committed int, res *Result) (*runner, error) {
	t0 := time.Now()
	verify := func(cp index.Checkpoint) bool {
		if cp.Chunk < 0 || cp.Chunk >= len(chunks) || cp.InputCursor != inPref[cp.Chunk+1] {
			return false
		}
		h := sha256.New()
		for _, raw := range chunks[cp.Chunk] {
			h.Write(raw)
		}
		if fmt.Sprintf("%x", h.Sum(nil)) != cp.ChunkHash {
			return false
		}
		got, n, err := index.HashPrefix(outPath, cp.OutputCursor)
		return err == nil && n == cp.OutputCursor && got == cp.OutputTailHash
	}
	cp, ok, err := store.Recover(verify)
	if err != nil {
		return nil, err
	}
	if !ok && committed > 0 {
		res.ExitCode = 1
		res.LastValidCheckpoint = -1
		return nil, nil
	}
	last := -1
	var cursor int64
	if ok {
		last, cursor = cp.Chunk, cp.OutputCursor
	}
	res.LastValidCheckpoint = last
	if err := os.Truncate(outPath, cursor); err != nil {
		return nil, err
	}
	nr := newRunner(cfg, chunks)
	nr.cursor = cursor
	if cursor > 0 {
		f, err := os.Open(outPath)
		if err != nil {
			return nil, err
		}
		_, err = io.CopyN(nr.outHash, f, cursor)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	for k := 0; k <= last; k++ {
		if err := nr.process(k, false, store); err != nil {
			return nil, err
		}
	}
	nr.out, err = os.OpenFile(outPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	for k := last + 1; k < len(chunks); k++ {
		if err := nr.process(k, true, store); err != nil {
			nr.out.Close()
			return nil, err
		}
	}
	nr.out.Close()
	res.ResumeMs = time.Since(t0).Milliseconds()
	return nr, nil
}

// corruptLastCheckpoint flips one hex digit of the newest checkpoint's
// output tail hash, leaving the JSON well-formed.
func corruptLastCheckpoint(s *index.Store) error {
	nums, err := s.List()
	if err != nil || len(nums) == 0 {
		return fmt.Errorf("pipeline: no checkpoint to corrupt")
	}
	p, err := s.Path(nums[len(nums)-1])
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	key := []byte(`"output_tail_hash":"`)
	i := bytes.Index(b, key)
	if i < 0 {
		return fmt.Errorf("pipeline: unexpected checkpoint format")
	}
	j := i + len(key)
	if b[j] == '0' {
		b[j] = '1'
	} else {
		b[j] = '0'
	}
	return os.WriteFile(p, b, 0o644)
}

type runner struct {
	cfg     Config
	chunks  [][][]byte
	inPref  []int64
	loss    *ingest.LossTracker
	reasm   *ingest.Reassembler
	merger  *timeline.Merger
	gen     *render.Generator
	active  []timeline.Event
	defects []defect.Defect
	out     *os.File
	outHash hash.Hash
	cursor  int64
}

func newRunner(cfg Config, chunks [][][]byte) *runner {
	inPref := make([]int64, len(chunks)+1)
	for i, c := range chunks {
		inPref[i+1] = inPref[i] + int64(len(c))
	}
	loss := &ingest.LossTracker{}
	return &runner{
		cfg: cfg, chunks: chunks, inPref: inPref,
		loss: loss, reasm: ingest.NewReassembler(loss),
		merger: timeline.NewMerger(), gen: render.NewGenerator(),
		outHash: sha256.New(),
	}
}

// process handles chunk k. With commit false it only advances internal
// state (fast-forward during recovery); nothing is written and the
// checkpoint is not touched.
func (r *runner) process(k int, commit bool, store *index.Store) error {
	h := sha256.New()
	var cues []subtitle.Cue
	var markers []timeline.Marker
	for _, raw := range r.chunks[k] {
		h.Write(raw)
		outs, err := r.reasm.Push(raw)
		if err != nil {
			r.defects = append(r.defects, defect.New(ingest.Track, -1, -1, defect.Malformed, err.Error()))
			continue
		}
		r.handlePayloads(outs, &cues, &markers)
	}
	if k == len(r.chunks)-1 {
		r.handlePayloads(r.reasm.Finish(), &cues, &markers)
	}
	horizon := int64(0)
	for _, c := range cues {
		if c.Start > horizon {
			horizon = c.Start
		}
	}
	for _, m := range markers {
		if m.Nanos > horizon {
			horizon = m.Nanos
		}
	}
	events := r.merger.AddChunk(cues, markers, horizon+1)
	r.active = append(r.active, events...)
	keep := r.active[:0]
	for _, ev := range r.active {
		if ev.End > horizon {
			keep = append(keep, ev)
		}
	}
	r.active = keep
	var w io.Writer = io.Discard
	if commit {
		w = countingWriter{w: io.MultiWriter(r.out, r.outHash), n: &r.cursor}
	}
	if err := r.gen.Advance(horizon+1, r.active, w); err != nil {
		return err
	}
	if !commit {
		return nil
	}
	return store.Commit(index.Checkpoint{
		Chunk:          k,
		InputCursor:    r.inPref[k+1],
		OutputCursor:   r.cursor,
		ChunkHash:      fmt.Sprintf("%x", h.Sum(nil)),
		OutputTailHash: fmt.Sprintf("%x", r.outHash.Sum(nil)),
	})
}

func (r *runner) handlePayloads(payloads [][]byte, cues *[]subtitle.Cue, markers *[]timeline.Marker) {
	for _, p := range payloads {
		c, m, err := parseRecord(p)
		if err != nil {
			r.defects = append(r.defects, defect.New(ingest.Track, -1, -1, defect.Malformed, err.Error()))
			continue
		}
		if c != nil {
			c.Index = len(*cues) + 1
			*cues = append(*cues, *c)
		}
		if m != nil {
			*markers = append(*markers, *m)
		}
	}
}

type countingWriter struct {
	w io.Writer
	n *int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	m, err := c.w.Write(p)
	*c.n += int64(m)
	return m, err
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
