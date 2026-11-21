package txn

import (
	"io"
	"os"

	"mediamerge/internal/model"
)

// copyInput records the exact input bytes inside the work directory so that a
// recovery run can re-derive the mapping without the original path. When the
// source already lives at the destination (the demo generates its input
// there) the copy is skipped rather than truncating the source.
func copyInput(dir, src string) error {
	output, _ := Paths(dir)
	dst := output[:len(output)-len("output.mp4")] + "input.mp4"
	if sameFile(src, dst) {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, 0, 0, false, err,
			"open input copy source")
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, 0, 0, false, err,
			"create input copy")
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, 0, 0, false, err,
			"copy input")
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, 0, 0, false, err,
			"sync input copy")
	}
	return out.Close()
}

func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(ia, ib)
}
