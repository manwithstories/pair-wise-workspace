package runner

import (
	"mediaarchive/internal/checkpoint"
	"mediaarchive/internal/model"
)

// loadState opens an existing checkpoint after verifying its input
// fingerprint, or starts a fresh state. A fingerprint mismatch is fatal.
func loadState(opts Options, inSHA string, inSize int64) (*checkpoint.State, error) {
	if opts.Checkpoint == "" {
		return checkpoint.NewState(opts.Input, inSHA, inSize), nil
	}
	st, err := checkpoint.Load(opts.Checkpoint, opts.Input, inSHA, inSize)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return checkpoint.NewState(opts.Input, inSHA, inSize), nil
	}
	if !opts.Resume {
		// A checkpoint exists but the run is not a resume: treat a stale
		// checkpoint as a parameter error rather than overwriting it.
		return nil, model.NewError(model.StageCheckpt,
			"checkpoint already exists; pass --resume to continue or remove it")
	}
	if st.Validated == nil {
		st.Validated = map[uint32][]checkpoint.SampleRecord{}
	}
	return st, nil
}
