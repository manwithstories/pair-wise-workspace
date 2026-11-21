package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediamerge/internal/fault"
)

// StageIndex maps pipeline stages to stable WAL stage identifiers.
type StageID uint16

const (
	StageParse StageID = 1
	StageAudio StageID = 2
	StageMux   StageID = 3
	StageFinal StageID = 4
)

func (s StageID) String() string {
	switch s {
	case StageParse:
		return "parse"
	case StageAudio:
		return "audio"
	case StageMux:
		return "mux"
	case StageFinal:
		return "final"
	default:
		return fmt.Sprintf("stage-%d", uint16(s))
	}
}

// StageState is what the log says about one stage.
type StageState int

const (
	// StatePending means the stage has not started.
	StatePending StageState = iota
	// StateOpen means a begin record exists without a commit.
	StateOpen
	// StateCommitted means the stage committed atomically.
	StateCommitted
)

// Recovery is the result of replaying a log against its output file.
type Recovery struct {
	// States maps each stage to its recovered state.
	States map[StageID]StageState
	// CommittedStages lists committed stage IDs in log order.
	CommittedStages []StageID
	// ResumeFrom is the first stage that must run again.
	ResumeFrom StageID
	// RolledBackBytes is the size of the uncommitted tail discarded from
	// the output file.
	RolledBackBytes int64
	// TruncatedLogBytes is the torn trailing WAL size that was dropped.
	TruncatedLogBytes int64
	// OutputBytes is the length of the output file after rollback.
	OutputBytes int64
	// Digests are the per-fragment output hashes of committed stages, in
	// commit order.
	Digests [][32]byte
}

// CommitManager appends stage records and drives atomic commits. Each stage
// writes into a same-volume staging file that already contains the committed
// prefix; Commit publishes it with an atomic rename, so the delivery file is
// never observed half-written.
type CommitManager struct {
	wal    *WAL
	output string
	states map[StageID]StageState
	// pending collects output fragments for the open stage.
	pending []OutputFragment
	open    StageID
	openSeq uint64
	// staged is the staging file for the open stage.
	staged *os.File
	// committedDigests preserves fragment hashes across commits.
	committedDigests [][32]byte
}

// OutputFragment is one written output range with its payload hash.
type OutputFragment struct {
	Offset int64
	Length int64
	Digest [32]byte
}

// NewCommitManager wraps a WAL and the output file it protects.
func NewCommitManager(wal *WAL, outputPath string) *CommitManager {
	return &CommitManager{
		wal:    wal,
		output: outputPath,
		states: map[StageID]StageState{},
	}
}

func (c *CommitManager) state(s StageID) StageState {
	if st, ok := c.states[s]; ok {
		return st
	}
	return StatePending
}

// Begin records the stage input digest and the input byte range. It also
// creates the stage's staging file, pre-seeded with the committed prefix, and
// returns it for the stage to write at absolute offsets. The caller must not
// close it; Commit or Rollback does.
func (c *CommitManager) Begin(s StageID, inDigest [32]byte, inBytes int64) error {
	_, err := c.begin(s, inDigest, inBytes)
	return err
}

// OpenStage is Begin plus the staging handle. The stage must write its bytes
// into the returned file at offsets at or beyond the committed prefix; the
// handle's current size is that boundary.
func (c *CommitManager) OpenStage(s StageID, inDigest [32]byte, inBytes int64) (*os.File, error) {
	return c.begin(s, inDigest, inBytes)
}

// Abandon closes the open stage's staging file without removing it, which
// models a process death: the WAL and the uncommitted staging bytes survive
// for the next recovery. It does not append a rollback record.
func (c *CommitManager) Abandon() {
	if c.staged != nil {
		c.staged.Close()
		c.staged = nil
	}
	c.pending = nil
	if c.open != 0 {
		c.states[c.open] = StateOpen
	}
	c.open = 0
}

// CommittedBoundary returns the output offset of the last commit record.
func (c *CommitManager) CommittedBoundary() int64 { return c.committedBoundary() }

func (c *CommitManager) begin(s StageID, inDigest [32]byte, inBytes int64) (*os.File, error) {
	if c.state(s) == StateCommitted {
		return nil, fault.New(fault.StageJournal, fault.CodeWALGap,
			fmt.Sprintf("stage %s is already committed", s))
	}
	if c.open != 0 && c.open != s {
		return nil, fault.New(fault.StageJournal, fault.CodeWALGap,
			fmt.Sprintf("stage %s is still open while beginning %s", c.open, s))
	}
	r, err := c.wal.Append(Record{
		Kind: KindBegin, Stage: uint16(s), Offset: inBytes, Digest: inDigest,
		Summary: fmt.Sprintf("in_bytes=%d", inBytes),
	})
	if err != nil {
		return nil, fault.Wrap(fault.StageJournal, fault.CodeWriteFailed, err, "append begin record")
	}
	staged, err := c.openStaging()
	if err != nil {
		return nil, err
	}
	c.open = s
	c.openSeq = r.Seq
	c.pending = nil
	c.staged = staged
	c.states[s] = StateOpen
	return staged, nil
}

// openStaging creates the staging file and copies the committed prefix into
// it. The returned handle is positioned at the prefix end.
func (c *CommitManager) openStaging() (*os.File, error) {
	boundary := c.committedBoundary()
	tmp := TempPath(c.output)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "create staging file")
	}
	if boundary > 0 {
		src, err := os.Open(c.output)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return nil, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "open committed output")
		}
		n, err := io.CopyN(f, src, boundary)
		src.Close()
		if err != nil || n != boundary {
			f.Close()
			os.Remove(tmp)
			return nil, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err,
				fmt.Sprintf("seed staging file with %d committed bytes", boundary))
		}
	}
	return f, nil
}

// AppendOutput records one output fragment. It must be called after Begin.
func (c *CommitManager) AppendOutput(s StageID, frag OutputFragment) error {
	return c.AppendOutputTrack(s, 0, frag)
}

// AppendOutputTrack records one output fragment together with the track that
// produced it. The track lets recovery rebuild per-track segment metrics.
func (c *CommitManager) AppendOutputTrack(s StageID, trackID int, frag OutputFragment) error {
	if c.state(s) != StateOpen || c.staged == nil {
		return fault.New(fault.StageJournal, fault.CodeWALGap,
			fmt.Sprintf("output for stage %s before begin", s))
	}
	r, err := c.wal.Append(Record{
		Kind: KindOutput, Stage: uint16(s), TrackID: trackID, Offset: frag.Offset, Digest: frag.Digest,
		Summary: fmt.Sprintf("len=%d", frag.Length),
	})
	if err != nil {
		return fault.Wrap(fault.StageJournal, fault.CodeWriteFailed, err, "append output record")
	}
	_ = r
	c.pending = append(c.pending, frag)
	return nil
}

// Commit atomically commits the open stage. It validates that the output file
// is at least as long as the last recorded fragment and that every pending
// fragment is present before writing the commit record. On success it fsyncs
// the output file first, so a commit record never describes bytes that are
// not durable.
func (c *CommitManager) Commit(s StageID, summary string) error {
	if c.state(s) != StateOpen {
		return fault.New(fault.StageJournal, fault.CodeWALGap,
			fmt.Sprintf("stage %s is not open", s))
	}
	var end int64
	for _, f := range c.pending {
		if f.Length < 0 || f.Offset < 0 {
			return fault.New(fault.StageJournal, fault.CodeStageFailed,
				fmt.Sprintf("stage %s has a negative fragment range", s))
		}
		if f.Offset+f.Length > end {
			end = f.Offset + f.Length
		}
	}
	st, err := c.staged.Stat()
	if err != nil {
		return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "stat staging file")
	}
	if end > st.Size() {
		return fault.New(fault.StageCommit, fault.CodeChecksumMismatch,
			fmt.Sprintf("stage %s declares output through byte %d but staging file has %d", s, end, st.Size()))
	}
	if err := c.staged.Sync(); err != nil {
		return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "fsync staging file")
	}
	for _, frag := range c.pending {
		ok, err := verifyFragment(c.staged.Name(), frag)
		if err != nil {
			return err
		}
		if !ok {
			return fault.New(fault.StageCommit, fault.CodeChecksumMismatch,
				fmt.Sprintf("stage %s fragment at %d+%d does not match its recorded hash", s, frag.Offset, frag.Length))
		}
	}
	if err := c.staged.Close(); err != nil {
		return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "close staging file")
	}
	if err := os.Rename(c.staged.Name(), c.output); err != nil {
		os.Remove(c.staged.Name())
		return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "atomic rename of staging file")
	}
	syncDir(c.output)
	c.staged = nil

	sum := summary
	if sum == "" {
		h := sha256.New()
		for _, f := range c.pending {
			h.Write(f.Digest[:])
		}
		sum = "digest=" + hex.EncodeToString(h.Sum(nil))
	}
	if _, err := c.wal.Append(Record{
		Kind: KindCommit, Stage: uint16(s), Offset: end, Summary: sum,
	}); err != nil {
		return fault.Wrap(fault.StageJournal, fault.CodeWriteFailed, err, "append commit record")
	}
	for _, f := range c.pending {
		c.committedDigests = append(c.committedDigests, f.Digest)
	}
	c.states[s] = StateCommitted
	c.pending = nil
	c.open = 0
	return nil
}

// syncDir fsyncs the directory holding path so the rename itself is durable.
func syncDir(path string) {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return
	}
	_ = d.Sync()
	d.Close()
}

// DiscardStaging removes a staging file left behind by a crashed commit.
func DiscardStaging(output string) error {
	err := os.Remove(TempPath(output))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func verifyFragment(path string, frag OutputFragment) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "open output")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, frag.Offset, frag.Length)); err != nil {
		return false, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "read output fragment")
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	return got == frag.Digest, nil
}

// Rollback closes an uncommitted tail: it appends a rollback record and
// truncates the output file to the last committed boundary. The byte count
// discarded is returned.
func (c *CommitManager) Rollback() (int64, error) {
	boundary := c.committedBoundary()
	var discarded int64
	if c.staged != nil {
		if st, err := c.staged.Stat(); err == nil && st.Size() > boundary {
			discarded = st.Size() - boundary
		}
		name := c.staged.Name()
		c.staged.Close()
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return discarded, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "discard staging file")
		}
		c.staged = nil
	}
	if st, err := os.Stat(c.output); err == nil && st.Size() > boundary {
		if err := os.Truncate(c.output, boundary); err != nil {
			return 0, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "truncate uncommitted tail")
		}
		discarded = st.Size() - boundary
	}
	if c.open != 0 {
		if _, err := c.wal.Append(Record{
			Kind: KindRollback, Stage: uint16(c.open), Offset: discarded,
			Summary: fmt.Sprintf("rolled_back=%d", discarded),
		}); err != nil {
			return discarded, fault.Wrap(fault.StageJournal, fault.CodeWriteFailed, err, "append rollback record")
		}
		c.states[c.open] = StatePending
		c.open = 0
		c.pending = nil
	}
	return discarded, nil
}

// committedBoundary recovers the output offset of the last commit record.
func (c *CommitManager) committedBoundary() int64 {
	records, _, err := c.wal.Read()
	if err != nil {
		return 0
	}
	var boundary int64
	for _, r := range records {
		if r.Kind == KindCommit {
			boundary = r.Offset
		}
	}
	return boundary
}

// verifyCommittedPrefix re-hashes every output fragment described by a
// commit record. Fragments belonging to stages whose commit was rolled back
// or never written are ignored because they are past the last boundary.
func verifyCommittedPrefix(outputPath string, records []Record) error {
	type frag struct {
		trackID int
		off     int64
		length  int64
		digest  [32]byte
	}
	var pending []frag
	var committed []frag
	for _, r := range records {
		switch r.Kind {
		case KindOutput:
			pending = append(pending, frag{
				trackID: r.TrackID, off: r.Offset, length: summaryLength(r.Summary), digest: r.Digest,
			})
		case KindCommit:
			committed = append(committed, pending...)
			pending = nil
		case KindRollback:
			pending = nil
		}
	}
	if len(committed) == 0 {
		return nil
	}
	f, err := os.Open(outputPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fault.New(fault.StageCommit, fault.CodeWALGap,
				"journal has committed stages but the output file is missing")
		}
		return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "open output")
	}
	defer f.Close()
	for i, fr := range committed {
		if fr.length <= 0 {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, fr.off, fr.length)); err != nil {
			return fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err,
				fmt.Sprintf("read committed fragment %d", i+1))
		}
		var got [32]byte
		copy(got[:], h.Sum(nil))
		if got != fr.digest {
			return fault.New(fault.StageCommit, fault.CodeChecksumMismatch,
				fmt.Sprintf("committed fragment %d at %d+%d does not match its journal hash (track %d)",
					i+1, fr.off, fr.length, fr.trackID))
		}
	}
	return nil
}

// summaryLength extracts the "len=N" value recorded with an output record.
func summaryLength(summary string) int64 {
	const prefix = "len="
	if len(summary) <= len(prefix) || summary[:len(prefix)] != prefix {
		return 0
	}
	var n int64
	for _, c := range summary[len(prefix):] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// CommittedDigests returns output fragment digests of all committed stages.
func (c *CommitManager) CommittedDigests() [][32]byte {
	out := make([][32]byte, len(c.committedDigests))
	copy(out, c.committedDigests)
	return out
}

// Recover replays a log against an output file. It validates sequence
// monotonicity, marks committed stages, detects an uncommitted tail and
// truncates the output back to the last commit boundary.
func Recover(walPath, outputPath string) (*Recovery, error) {
	f, err := os.Open(walPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Recovery{States: map[StageID]StageState{}, ResumeFrom: StageParse}, nil
		}
		return nil, fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "open wal")
	}
	records, tail, err := ReadAll(f)
	f.Close()
	if err != nil {
		return nil, fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, "read wal")
	}
	rec := &Recovery{States: map[StageID]StageState{}, TruncatedLogBytes: tail.TruncatedBytes}
	var boundary int64
	var lastSeq uint64
	openStages := map[StageID]bool{}
	for _, r := range records {
		if r.Seq != lastSeq+1 {
			return nil, fault.New(fault.StageJournal, fault.CodeWALGap,
				fmt.Sprintf("record sequence %d does not follow %d", r.Seq, lastSeq))
		}
		lastSeq = r.Seq
		s := StageID(r.Stage)
		switch r.Kind {
		case KindBegin:
			openStages[s] = true
			rec.States[s] = StateOpen
		case KindOutput:
			if !openStages[s] {
				return nil, fault.New(fault.StageJournal, fault.CodeWALGap,
					fmt.Sprintf("output record for %s without begin", s))
			}
			var d [32]byte
			copy(d[:], r.Digest[:])
			openStages[s] = true
		case KindCommit:
			if !openStages[s] {
				return nil, fault.New(fault.StageJournal, fault.CodeWALGap,
					fmt.Sprintf("commit record for %s without begin", s))
			}
			openStages[s] = false
			rec.States[s] = StateCommitted
			rec.CommittedStages = append(rec.CommittedStages, s)
			boundary = r.Offset
		case KindRollback:
			openStages[s] = false
			rec.States[s] = StatePending
		}
	}
	for s, open := range openStages {
		if open {
			rec.States[s] = StateOpen
		}
	}

	// A commit record promises that every fragment it describes is on disk
	// with the recorded hash. Re-check the committed prefix so silent
	// corruption is found during recovery, not after delivery.
	if err := verifyCommittedPrefix(outputPath, records); err != nil {
		return nil, err
	}

	// Resume at the first stage after the committed prefix.
	rec.ResumeFrom = StageParse
	for _, s := range []StageID{StageParse, StageAudio, StageMux, StageFinal} {
		if rec.States[s] == StateCommitted {
			rec.ResumeFrom = s + 1
		} else {
			break
		}
	}

	st, err := os.Stat(outputPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if tmpStat, terr := os.Stat(TempPath(outputPath)); terr == nil {
		// A crash during a stage leaves a staging file that was never
		// renamed. Everything past the last commit boundary is discarded;
		// the prefix it shares with the output is not counted twice.
		extra := tmpStat.Size() - boundary
		if extra > 0 {
			rec.RolledBackBytes += extra
		}
		if err := DiscardStaging(outputPath); err != nil {
			return nil, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "discard staging file")
		}
	}
	if st != nil {
		rec.OutputBytes = st.Size()
		if st.Size() > boundary {
			rec.RolledBackBytes = st.Size() - boundary
			if err := os.Truncate(outputPath, boundary); err != nil {
				return nil, fault.Wrap(fault.StageCommit, fault.CodeWriteFailed, err, "rollback truncate")
			}
			rec.OutputBytes = boundary
		} else if st.Size() < boundary {
			return nil, fault.New(fault.StageCommit, fault.CodeWALGap,
				fmt.Sprintf("log commits through byte %d but output is only %d", boundary, st.Size()))
		}
	}

	// Append a rollback record for stages left open so a second restart does
	// not see them as open again.
	needsRollback := false
	for _, st := range rec.States {
		if st == StateOpen {
			needsRollback = true
		}
	}
	if needsRollback {
		w, err := OpenWAL(walPath)
		if err != nil {
			return nil, err
		}
		for s, st := range rec.States {
			if st != StateOpen {
				continue
			}
			if _, err := w.Append(Record{Kind: KindRollback, Stage: uint16(s), Offset: rec.RolledBackBytes,
				Summary: fmt.Sprintf("recovered_rollback=%d", rec.RolledBackBytes)}); err != nil {
				w.Close()
				return nil, err
			}
			rec.States[s] = StatePending
		}
		w.Close()
	}
	return rec, nil
}
