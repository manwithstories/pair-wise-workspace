package probe

import (
	"io"

	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
)

// maxSampleRead caps a single sample read; forged sizes are clamped before
// any allocation so a corrupt index cannot trigger huge memory use.
const maxSampleRead = 4 << 20 // 4 MiB per sample

// VerifySamples reads each declared sample through the mdat-bounded reader
// and validates the synthetic CRC. It returns the set of invalid sample
// indices (0-based) and model-level errors carrying shared tick ranges.
// Reads never cross the mdat boundary.
func VerifySamples(r io.ReaderAt, res *Result, tr *Track) (map[uint32]struct{}, []*model.MediaError) {
	invalid := map[uint32]struct{}{}
	var errs []*model.MediaError
	if res.Integrity.Mdat == nil {
		return invalid, errs
	}
	br := boundedSampleReader(r, res.Integrity.Mdat)
	for _, s := range tr.Samples {
		size := s.Size
		if size <= 0 || int64(size) > maxSampleRead {
			invalid[s.Index] = struct{}{}
			errs = append(errs, model.NewError(model.CodeLengthOutOfBounds,
				res.InputID, s.StartTicks, s.EndTicks,
				"sample size missing or exceeds per-sample cap"))
			continue
		}
		buf := make([]byte, size)
		if _, err := br.ReadAt(buf, s.ChunkOffset); err != nil {
			invalid[s.Index] = struct{}{}
			errs = append(errs, model.NewError(model.CodeContainerTruncated,
				res.InputID, s.StartTicks, s.EndTicks,
				"sample bytes unreadable within mdat boundary"))
			continue
		}
		if _, err := fixture.VerifySample(buf, s.Index); err != nil {
			invalid[s.Index] = struct{}{}
			errs = append(errs, model.NewError(model.CodeCorruptSample,
				res.InputID, s.StartTicks, s.EndTicks, err.Error()))
		}
	}
	return invalid, errs
}
