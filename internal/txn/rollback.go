package txn

import (
	"os"
	"path/filepath"

	"relaycheck/internal/media"
)

// DiscardStaging removes all uncommitted "*.tmp" fragments and the temporary
// manifest. Committed ".rlc" files and the WAL are left untouched. It is the
// allowed alternative to replaying a prepared transaction: anything not yet
// atomically renamed was never part of the committed set.
func (c *Committer) DiscardStaging() (int, *media.Fault) {
	removed := 0

	segTmps, err := filepath.Glob(filepath.Join(c.SegDir, "*.tmp"))
	if err != nil {
		return removed, media.NewFault(media.CodeCommitFailed, "glob staging: %v", err)
	}
	for _, p := range segTmps {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return removed, media.NewFault(media.CodeCommitFailed, "discard %s: %v", p, err)
		}
		removed++
	}

	manifestTmp := filepath.Join(c.Dir, "manifest.json.tmp")
	if err := os.Remove(manifestTmp); err != nil && !os.IsNotExist(err) {
		return removed, media.NewFault(media.CodeCommitFailed, "discard manifest tmp: %v", err)
	}
	return removed, nil
}

// IsCommitted reports whether the final fragment for index exists.
func (c *Committer) IsCommitted(index int) bool {
	_, err := os.Stat(c.finalPath(index))
	return err == nil
}
