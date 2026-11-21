package txn

import (
	"context"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/mux"
)

// ResumeResult reports a resumed run.
type ResumeResult struct {
	RecoveredStage int
	RollbackBytes  int64
	OutputSHA      string
	OutputLen      int64
	Samples        int
	SampleMatch    bool
	AlreadyFinal   bool
}

// Resume recovers the transaction in dir and continues writing from the last
// complete commit boundary. Because every sample position is fixed by the
// layout, the resumed output is byte-identical to an uninterrupted run.
func Resume(ctx context.Context, dir string, r *demux.Reader, plan *mapping.Plan, queue int) (*ResumeResult, error) {
	st, err := Recover(dir, r.InputSHA, r.Size, plan)
	if err != nil {
		return nil, err
	}
	if st.CommittedStage < 0 {
		return nil, model.NewError(model.StageRecovery, model.CodeIntegrity, st.CommittedLen, 0, false,
			"journal has no committed stage")
	}
	output, _ := Paths(dir)
	final := len(plan.Tracks) + 1
	if st.CommittedStage >= final {
		// Recovery already validated the complete output; a repeated recovery
		// must be a strict no-op.
		res := &ResumeResult{RecoveredStage: st.CommittedStage, RollbackBytes: st.RollbackBytes,
			AlreadyFinal: true}
		sha, size, err := checksum.File(output)
		if err != nil {
			return nil, err
		}
		res.OutputSHA, res.OutputLen = sha, size
		if err := verifyResume(dir, r, plan, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	m, err := New(dir, plan, r.InputSHA, r.Size)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	if err := m.attachLog(); err != nil {
		return nil, err
	}
	w, _, rolled, err := mux.OpenAt(m.Output, plan, st.CommittedLen)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	res := &ResumeResult{RecoveredStage: st.CommittedStage, RollbackBytes: st.RollbackBytes + rolled}
	if err := m.Begin(st.CommittedStage, st.CommittedLen); err != nil {
		return nil, err
	}
	{
		for ti := 0; ti < len(plan.Tracks); ti++ {
			if w.NextSample(ti) >= len(plan.Tracks[ti].Samples) {
				continue
			}
			stage := ti + 1
			if stage > st.CommittedStage {
				if err := m.Begin(stage, w.Size()); err != nil {
					return nil, err
				}
			}
			if err := resumeTrack(ctx, w, r, plan, ti); err != nil {
				return nil, err
			}
			if err := w.Sync(); err != nil {
				return nil, err
			}
			if err := m.CommitSynced(stage, w.Size()); err != nil {
				return nil, err
			}
		}
		if err := m.Begin(final, w.Size()); err != nil {
			return nil, err
		}
		if _, err := w.Finalize(); err != nil {
			return nil, err
		}
		if err := m.CommitSynced(final, w.Size()); err != nil {
			return nil, err
		}
	}
	sha, size, err := checksum.File(m.Output)
	if err != nil {
		return nil, err
	}
	res.OutputSHA, res.OutputLen = sha, size
	if err := verifyResume(dir, r, plan, res); err != nil {
		return nil, err
	}
	return res, nil
}

// verifyResume re-parses the output and compares per-track hashes against the
// original input samples.
func verifyResume(dir string, in *demux.Reader, plan *mapping.Plan, res *ResumeResult) error {
	output, _ := Paths(dir)
	rr, err := demux.Open(context.Background(), output)
	if err != nil {
		return err
	}
	defer rr.Close()
	if len(rr.Tracks) != len(plan.Tracks) {
		return model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
			"output has %d tracks, expected %d", len(rr.Tracks), len(plan.Tracks))
	}
	res.SampleMatch = true
	for i, tr := range plan.Tracks {
		h := checksum.NewSampleHasher()
		for _, ref := range rr.Tracks[i].Samples {
			data, err := rr.ReadSample(ref)
			if err != nil {
				return err
			}
			h.Add(data)
		}
		want := checksum.NewSampleHasher()
		for _, ref := range in.Tracks[i].Samples {
			data, err := in.ReadSample(ref)
			if err != nil {
				return err
			}
			want.Add(data)
		}
		if h.Sum() != want.Sum() || h.Count() != len(tr.Samples) {
			res.SampleMatch = false
		}
		res.Samples += h.Count()
	}
	if !res.SampleMatch {
		return model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
			"per-track sample digests do not match the input")
	}
	return nil
}

func resumeTrack(ctx context.Context, w *mux.Writer, r *demux.Reader, plan *mapping.Plan, ti int) error {
	tr := plan.Tracks[ti]
	in := r.Tracks[ti]
	for si := w.NextSample(ti); si < len(tr.Samples); si++ {
		if err := ctx.Err(); err != nil {
			return model.NewError(model.StageRecovery, model.CodeCanceled, in.Samples[si].Offset,
				tr.InputID, true, "resume canceled")
		}
		data, err := r.ReadSample(in.Samples[si])
		if err != nil {
			return err
		}
		if err := w.WriteSample(ti, model.Sample{
			TrackIndex: ti, TrackID: tr.TargetID, Index: si, Offset: in.Samples[si].Offset,
			DTS: in.Samples[si].DTS, Duration: in.Samples[si].Duration,
			CTS: in.Samples[si].CTSOffset, Sync: in.Samples[si].Sync, Data: data,
		}); err != nil {
			return err
		}
	}
	return nil
}
