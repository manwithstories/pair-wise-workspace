package safe

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"bcast/internal/fault"
)

// CommitTemp hashes and fsyncs a fully written temporary file, then atomically
// renames it to its final confined location. The returned digest is calculated
// before the commit point.
func (r *Root) CommitTemp(tempRel, finalRel string) (string, error) {
	tmp, err := r.Resolve(tempRel)
	if err != nil {
		return "", err
	}
	final, err := r.Resolve(finalRel)
	if err != nil {
		return "", err
	}
	if filepath.Dir(tmp) != filepath.Dir(final) {
		return "", fault.E(fault.WriteFailed, "safe", "CommitTemp", nil).WithPath(tempRel)
	}

	f, err := os.Open(tmp)
	if err != nil {
		return "", fault.E(fault.WriteFailed, "safe", "CommitTemp.open", err).WithPath(tempRel)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return "", fault.E(fault.WriteFailed, "safe", "CommitTemp.hash", err).WithPath(tempRel)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fault.E(fault.WriteFailed, "safe", "CommitTemp.sync", err).WithPath(tempRel)
	}
	if err := f.Close(); err != nil {
		return "", fault.E(fault.WriteFailed, "safe", "CommitTemp.close", err).WithPath(tempRel)
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", fault.E(fault.WriteFailed, "safe", "CommitTemp.rename", err).WithPath(finalRel)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyHash streams a committed file and compares its SHA-256.
func (r *Root) VerifyHash(rel, want string) error {
	p, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return fault.E(fault.EvidenceMismatch, "safe", "VerifyHash.open", err).WithPath(rel)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fault.E(fault.EvidenceMismatch, "safe", "VerifyHash.read", err).WithPath(rel)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fault.E(fault.EvidenceMismatch, "safe", "VerifyHash",
			&HashMismatch{Rel: rel, Want: want, Got: got}).WithPath(rel)
	}
	return nil
}

// HashFile returns a confined file's digest.
func (r *Root) HashFile(rel string) (string, error) {
	p, err := r.Resolve(rel)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fault.E(fault.Internal, "safe", "HashFile", err).WithPath(rel)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fault.E(fault.Internal, "safe", "HashFile.read", err).WithPath(rel)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashMismatch carries both digests for locatable evidence errors.
type HashMismatch struct {
	Rel  string
	Want string
	Got  string
}

func (e *HashMismatch) Error() string {
	return "sha256 mismatch for " + e.Rel + ": want " + e.Want + " got " + e.Got
}
