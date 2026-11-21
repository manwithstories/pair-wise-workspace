// Package verify re-reads a produced directory and checks that journal, input
// fingerprint, output length and per-track sample digests all agree.
package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/journal"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/txn"
)

// Report is the verified state of a work directory.
type Report struct {
	InputSHA    string
	OutputSHA   string
	Tracks      int
	Samples     int
	SampleMatch bool
	Final       bool
	Stage       int
	InputSize   int64
	OutputLen   int64
}

// Summary renders the stable machine-readable line.
func (r *Report) Summary() string {
	return fmt.Sprintf("tracks=%d samples=%d input_sha256=%s output_sha256=%s sample_sha256_match=%t",
		r.Tracks, r.Samples, r.InputSHA, r.OutputSHA, r.SampleMatch)
}

// Run verifies dir. An inconsistency is returned as a model.Error with code
// integrity so the CLI exits 4.
func Run(ctx context.Context, dir string) (*Report, error) {
	input := filepath.Join(dir, "input.mp4")
	output, jpath := txn.Paths(dir)
	rep := &Report{}
	if !within(dir, input) || !within(dir, output) || !within(dir, jpath) {
		return nil, model.NewError(model.StageVerify, model.CodePath, 0, 0, false,
			"work directory escaped: %s", dir)
	}
	inSHA, inSize, err := checksum.File(input)
	if err != nil {
		return nil, model.WrapError(model.StageVerify, model.CodeIntegrity, 0, 0, false, err,
			"hash input")
	}
	rep.InputSHA, rep.InputSize = inSHA, inSize
	recs, err := journal.Load(jpath)
	if err != nil {
		return nil, err
	}
	for i, r := range recs {
		if r.InputSHA256 != inSHA || r.InputSize != inSize {
			return nil, model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
				"journal record %d does not match input fingerprint", i)
		}
	}
	in, err := demux.Open(ctx, input)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	plan, err := mapping.Apply(in.Tracks, mapping.DefaultRules(in.Tracks))
	if err != nil {
		return nil, err
	}
	rep.Tracks = len(plan.Tracks)
	st, err := txn.Recover(dir, inSHA, inSize, plan)
	if err != nil {
		return nil, err
	}
	rep.Stage = st.CommittedStage
	rep.Final = st.CommittedStage == len(plan.Tracks)+1
	if !rep.Final {
		return nil, model.NewError(model.StageVerify, model.CodeJournal, st.CommittedLen, 0, false,
			"output is not final: last committed stage %d of %d", st.CommittedStage, len(plan.Tracks)+1)
	}
	outSHA, outSize, err := checksum.File(output)
	if err != nil {
		return nil, model.WrapError(model.StageVerify, model.CodeIntegrity, 0, 0, false, err,
			"hash output")
	}
	rep.OutputSHA, rep.OutputLen = outSHA, outSize
	if got := recs[len(recs)-1-3]; got.OutputLen > outSize {
		return nil, model.NewError(model.StageVerify, model.CodeIntegrity, outSize, 0, false,
			"journal claims %d bytes, output is %d", got.OutputLen, outSize)
	}
	out, err := demux.Open(ctx, output)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if len(out.Tracks) != len(plan.Tracks) {
		return nil, model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
			"output has %d tracks, expected %d", len(out.Tracks), len(plan.Tracks))
	}
	rep.SampleMatch = true
	for ti := range plan.Tracks {
		if out.Tracks[ti].TargetID != plan.Tracks[ti].TargetID {
			return nil, model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
				"track %d target id %d, expected %d", ti, out.Tracks[ti].TargetID, plan.Tracks[ti].TargetID)
		}
		if len(out.Tracks[ti].Samples) != len(plan.Tracks[ti].Samples) {
			return nil, model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
				"track %d has %d samples, expected %d", ti,
				len(out.Tracks[ti].Samples), len(plan.Tracks[ti].Samples))
		}
		want := checksum.NewSampleHasher()
		for _, ref := range plan.Tracks[ti].Samples {
			data, err := in.ReadSample(ref)
			if err != nil {
				return nil, err
			}
			want.Add(data)
		}
		got := checksum.NewSampleHasher()
		for _, ref := range out.Tracks[ti].Samples {
			data, err := out.ReadSample(ref)
			if err != nil {
				return nil, err
			}
			got.Add(data)
		}
		if want.Sum() != got.Sum() {
			rep.SampleMatch = false
			return nil, model.NewError(model.StageVerify, model.CodeIntegrity, 0, plan.Tracks[ti].TargetID,
				false, "track %d compressed sample digest %s != input %s",
				plan.Tracks[ti].TargetID, got.Sum(), want.Sum())
		}
		rep.Samples += got.Count()
	}
	return rep, nil
}

// within reports whether path stays inside root after cleaning.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && len(rel) > 0 && rel[0] != '.' || len(rel) > 2 && rel[:2] != "..")
}

// EnsureDir creates dir and rejects symlinked work directories.
func EnsureDir(dir string) error {
	info, err := os.Lstat(dir)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return model.NewError(model.StageVerify, model.CodePath, 0, 0, false,
			"work directory is a symlink: %s", dir)
	}
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	return err
}
