package txn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/mux"
	"mediamerge/internal/stream"
)

// ErrInterrupted is returned when the run simulated a crash at a requested
// stage. The journal and output prefix remain on disk for recovery.
var ErrInterrupted = errors.New("txn: run interrupted for recovery test")

// Options controls a pipeline run.
type Options struct {
	Queue          int
	InterruptAfter int // commit this stage, then simulate a crash; -1 disables
	CrashBytes     int // uncommitted bytes to append before dying; <0 means half a track
}

// TrackDigest reports per-track lossless verification.
type TrackDigest struct {
	TargetID   uint32
	Samples    int
	InputHash  string
	OutputHash string
	Match      bool
}

// Result summarizes one run.
type Result struct {
	InputSHA    string
	OutputSHA   string
	OutputLen   int64
	Samples     int
	Tracks      int
	SampleMatch bool
	Digests     []TrackDigest
	Interrupted bool
	QueuePeak   int
	StageMS     []int64
}

// Run executes the full pipeline into dir with the default file names and
// produces the recovery fixture (input copy + journal + output) that the
// recover command consumes.
func Run(ctx context.Context, dir string, r *demux.Reader, plan *mapping.Plan, opts Options) (*Result, error) {
	output, _ := Paths(dir)
	if err := copyInput(dir, r.Path); err != nil {
		return nil, err
	}
	return Merge(ctx, dir, output, r, plan, opts)
}

// Merge executes demux→mapping→bounded stream→remux into an explicit output
// path, journaling every stage inside dir.
func Merge(ctx context.Context, dir, output string, r *demux.Reader, plan *mapping.Plan, opts Options) (*Result, error) {
	if opts.Queue < 1 {
		opts.Queue = 1
	}
	if opts.InterruptAfter == 0 {
		opts.InterruptAfter = -1
	}
	m, err := New(dir, plan, r.InputSHA, r.Size)
	if err != nil {
		return nil, err
	}
	m.Output = output
	defer m.Close()
	w, _, err := mux.New(output, plan)
	if err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			w.Close()
		}
	}()
	res := &Result{InputSHA: r.InputSHA, Tracks: len(plan.Tracks), SampleMatch: true}
	inHashers := make([]*checksum.SampleHasher, len(plan.Tracks))
	for i := range inHashers {
		inHashers[i] = checksum.NewSampleHasher()
	}
	if err := m.Begin(0, w.Size()); err != nil {
		return nil, err
	}
	if err := m.CommitFile(0, w.Size()); err != nil {
		return nil, err
	}
	for ti, tr := range plan.Tracks {
		stage := ti + 1
		if err := m.Begin(stage, w.Size()); err != nil {
			return nil, err
		}
		wall := time.Now()
		peak, err := streamTrack(ctx, w, r, tr, ti, opts.Queue, inHashers[ti])
		if peak > res.QueuePeak {
			res.QueuePeak = peak
		}
		if err != nil {
			return nil, err
		}
		if err := w.Sync(); err != nil {
			return nil, err
		}
		if err := m.CommitSynced(stage, w.Size()); err != nil {
			return nil, err
		}
		res.Samples += len(tr.Samples)
		res.StageMS = append(res.StageMS, time.Since(wall).Milliseconds())
		if opts.InterruptAfter == stage {
			if err := injectCrash(m, w, r, plan, stage, opts.CrashBytes); err != nil {
				return nil, err
			}
			w.Close()
			closed = true
			res.Interrupted = true
			return res, ErrInterrupted
		}
	}
	final := len(plan.Tracks) + 1
	if err := m.Begin(final, w.Size()); err != nil {
		return nil, err
	}
	total, err := w.Finalize()
	if err != nil {
		return nil, err
	}
	closed = true
	if err := m.CommitSynced(final, total); err != nil {
		return nil, err
	}
	res.OutputLen = total
	outSHA, _, err := checksum.File(output)
	if err != nil {
		return nil, err
	}
	res.OutputSHA = outSHA
	if err := verifySamples(dir, output, plan, r, inHashers, res); err != nil {
		return nil, err
	}
	return res, nil
}

// injectCrash writes uncommitted bytes after a committed stage so that
// recovery has something real to roll back.
func injectCrash(m *Manager, w *mux.Writer, r *demux.Reader, plan *mapping.Plan, stage, crashBytes int) error {
	if stage >= len(plan.Tracks) {
		return nil
	}
	next := plan.Tracks[stage]
	if err := m.Begin(stage+1, w.Size()); err != nil {
		return err
	}
	n := crashBytes
	if n < 0 {
		n = len(next.Samples) / 2
	}
	for k := 0; k < n && k < len(next.Samples); k++ {
		data, err := r.ReadSample(next.Samples[k])
		if err != nil {
			return err
		}
		if _, err := w.WritePartial(data); err != nil {
			return err
		}
	}
	return w.Sync()
}

// streamTrack pushes one track through a bounded pipe. The producer blocks on
// a full queue (backpressure); the consumer writes samples in order. Any write
// or cancellation error fails the pipe so the producer cannot run ahead.
func streamTrack(ctx context.Context, w *mux.Writer, r *demux.Reader, tr *demux.Track, ti, queue int, hasher *checksum.SampleHasher) (int, error) {
	return streamRange(ctx, w, r, tr, ti, 0, queue, hasher)
}

// streamRange is streamTrack bounded to samples [start, end). Resume uses it
// so the queue capacity and backpressure guarantees hold on the recovery path
// too.
func streamRange(ctx context.Context, w *mux.Writer, r *demux.Reader, tr *demux.Track, ti, start, queue int, hasher *checksum.SampleHasher) (int, error) {
	if start < 0 {
		start = 0
	}
	if start > len(tr.Samples) {
		start = len(tr.Samples)
	}
	p := stream.New(ctx, queue)
	defer p.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer p.CloseSend()
		for _, ref := range tr.Samples[start:] {
			if err := ctx.Err(); err != nil {
				e := model.NewError(model.StageStream, model.CodeCanceled, ref.Offset, tr.InputID, true,
					"producer canceled: %v", err)
				p.Fail(e)
				return
			}
			data, err := r.ReadSample(ref)
			if err != nil {
				p.Fail(err)
				return
			}
			hasher.Add(data)
			s := model.Sample{
				TrackIndex: ti, TrackID: tr.TargetID, Index: ref.Index, Offset: ref.Offset,
				DTS: ref.DTS, Duration: ref.Duration, CTS: ref.CTSOffset, Sync: ref.Sync, Data: data,
			}
			if err := p.Send(s); err != nil {
				return
			}
		}
	}()
	var firstErr error
	peak := 0
	for {
		s, ok, err := p.Recv()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			p.Fail(err)
			break
		}
		if !ok {
			break
		}
		if st := p.Stats(); st.Resident > peak {
			peak = st.Resident
		}
		if err := w.WriteSample(ti, s); err != nil {
			firstErr = err
			p.Fail(err)
			break
		}
		p.SetBoundary(model.Boundary{
			SampleIndex: s.Index, TrackIndex: ti, TrackID: s.TrackID,
			TrackSample: s.Index, OutputBytes: w.Size(),
		})
	}
	wg.Wait()
	if firstErr != nil {
		return peak, firstErr
	}
	if err := p.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return peak, err
	}
	return peak, nil
}

// verifySamples re-reads the output and compares per-track compressed sample
// hashes against what was read from the input.
func verifySamples(dir, output string, plan *mapping.Plan, _ *demux.Reader, inHashers []*checksum.SampleHasher, res *Result) error {
	r, err := demux.Open(context.Background(), output)
	if err != nil {
		return err
	}
	defer r.Close()
	if len(r.Tracks) != len(plan.Tracks) {
		return model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
			"output has %d tracks, expected %d", len(r.Tracks), len(plan.Tracks))
	}
	for ti, tr := range plan.Tracks {
		h := checksum.NewSampleHasher()
		ot := r.Tracks[ti]
		for _, ref := range ot.Samples {
			data, err := r.ReadSample(ref)
			if err != nil {
				return err
			}
			h.Add(data)
		}
		d := TrackDigest{TargetID: tr.TargetID, Samples: len(tr.Samples),
			InputHash: inHashers[ti].Sum(), OutputHash: h.Sum()}
		d.Match = d.InputHash == d.OutputHash && h.Count() == len(tr.Samples)
		if !d.Match {
			res.SampleMatch = false
		}
		res.Digests = append(res.Digests, d)
	}
	return nil
}

// FormatSummary renders the deterministic summary line.
func (r *Result) FormatSummary() string {
	return fmt.Sprintf("tracks=%d samples=%d input_sha256=%s output_sha256=%s sample_sha256_match=%t",
		r.Tracks, r.Samples, r.InputSHA, r.OutputSHA, r.SampleMatch)
}
