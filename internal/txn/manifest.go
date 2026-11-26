package txn

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediamix/internal/clock"
)

// loadCommits reads every committed manifest in dir into the coordinator,
// ordered by sequence, and advances the seq cursor past the highest one.
func (c *Commit) loadCommits() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return clock.WrapError(clock.StageCommit, clock.ErrInvalidPath, 0, false, "read dir", err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != commitSuffix {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(c.dir, name))
		if err != nil {
			return clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false, "read manifest", err)
		}
		var m CommitManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false, "decode manifest", err)
		}
		c.committed = append(c.committed, m)
		if m.Seq > c.seq {
			c.seq = m.Seq
		}
	}
	// Order manifests by sequence.
	for i := 0; i < len(c.committed); i++ {
		for j := i + 1; j < len(c.committed); j++ {
			if c.committed[j].Seq < c.committed[i].Seq {
				c.committed[i], c.committed[j] = c.committed[j], c.committed[i]
			}
		}
	}
	return nil
}

// manifestName returns the on-disk manifest path component for a seq.
func manifestName(seq uint64) string {
	return "commit-" + itoa(seq) + commitSuffix
}

// itoa formats a uint64 without pulling in strconv's larger dependency path.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// syncDir fsyncs a directory so a rename within it is crash-durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false, "open dir for fsync", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false, "fsync dir", err)
	}
	return nil
}
