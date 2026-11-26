package pipeline

import (
	"crypto/sha256"
	"encoding/hex"

	"mediamix/internal/txn"
)

// rebuildOutput reconstructs the delivered byte stream from durable WAL data
// records referenced by committed manifests. This is the same code path a
// restart after a crash would take, so a replayed run hashes identically to
// a fault-free run.
func (r *Runner) rebuildOutput(committer *txn.Commit) ([]byte, error) {
	return committer.CommittedPayload()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
