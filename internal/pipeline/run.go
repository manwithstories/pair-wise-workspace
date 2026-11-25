package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"archivectl/internal/execute"
	"archivectl/internal/layout"
	"archivectl/internal/repair"
)

// ErrInterrupted is returned when the FailAfter injection fires.
var ErrInterrupted = errors.New("pipeline: interrupted after configured chunks")

// Options controls a run.
type Options struct {
	// FailAfter, when >0, makes the run stop after this many newly
	// written chunks, simulating a crash for resume testing.
	FailAfter int
}

// Report summarizes a completed group run.
type Report struct {
	Group             string  `json:"group"`
	IndexSHA256       string  `json:"index_sha256"`
	InputSHA256       string  `json:"input_sha256"`
	ChunkCount        int     `json:"chunk_count"`
	Written           int     `json:"written"`
	Skipped           int     `json:"skipped"`
	AVSyncMaxMS       float64 `json:"av_sync_max_ms"`
	LossRecovery      float64 `json:"loss_recovery"`
	UnrecoveredGaps   int     `json:"unrecovered_gaps"`
	Duplicates        int     `json:"rtp_duplicates"`
	Reordered         int     `json:"rtp_reordered"`
}

// Run scans/repairs inputs, streams every chunk atomically and saves the
// canonical index. Existing complete chunks are skipped, making Run
// safe to resume.
func Run(root, group string, opts Options) (*Report, error) {
	a, err := Analyze(root, group)
	if err != nil {
		return nil, err
	}
	out := layout.OutDir(root, group)
	_ = execute.CleanStaleTemp(out)
	for _, t := range layout.Tracks {
		_ = execute.CleanStaleTemp(filepath.Join(out, t))
	}

	rep := &Report{Group: group, InputSHA256: a.InputDigest}
	newWrites := 0
	for _, track := range layout.Tracks {
		tl := a.Timelines[track]
		for _, r := range tl.Ranges {
			i := r.SegmentIndex
			src := layout.SourcePath(root, group, track, i)
			dst := layout.OutputPath(root, group, track, i)
			st, err := os.Stat(src)
			if err != nil {
				return nil, err
			}
			if execute.ChunkComplete(dst, st.Size()) {
				rep.Skipped++
				rep.ChunkCount++
				continue
			}
			frag := a.Fragments[track][i]
			patch, err := execute.TFDTYPatch(frag.TFDT, r.CorrectedNativeBase)
			if err != nil {
				return nil, err
			}
			if _, _, err := execute.RewriteFileAtomic(src, dst, []execute.Patch{patch}); err != nil {
				return nil, err
			}
			rep.Written++
			newWrites++
			rep.ChunkCount++
			if opts.FailAfter > 0 && newWrites >= opts.FailAfter {
				return rep, ErrInterrupted
			}
		}
	}

	idx, a2, err := BuildIndex(root, group)
	if err != nil {
		return nil, err
	}
	digest, err := idx.SaveAtomic(layout.IndexPath(root, group))
	if err != nil {
		return nil, err
	}
	rep.IndexSHA256 = digest
	rep.ChunkCount = len(idx.Chunks)
	if a2 != nil && a2.RTP != nil {
		rep.LossRecovery = a2.RTP.LossRecovery
		rep.UnrecoveredGaps = len(a2.RTP.UnrecoveredGaps)
		rep.Duplicates = a2.RTP.Duplicates
		rep.Reordered = a2.RTP.Reordered
	}
	if vt, at := a.Timelines["video"], a.Timelines["audio"]; vt != nil && at != nil {
		rep.AVSyncMaxMS = repair.MaxAVSyncMillis(vt, at)
	}
	return rep, nil
}

// ResumeCheck proves crash recovery: it starts clean, injects an
// interruption plus a stale half-file, then resumes and requires the
// final run to skip completed chunks and leave no temp files.
func ResumeCheck(root, group string, segments int) (*Report, error) {
	_ = os.RemoveAll(layout.OutDir(root, group))
	partial, err := Run(root, group, Options{FailAfter: 4})
	if !errors.Is(err, ErrInterrupted) {
		return nil, fmt.Errorf("resume: expected interruption, got partial=%v err=%v", partial != nil, err)
	}
	// Simulate a half-written temp product left by a killed process.
	stale := layout.OutputPath(root, group, "video", segments-1) + ".crash.tmp"
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(stale, []byte("half"), 0o644); err != nil {
		return nil, err
	}
	final, err := Run(root, group, Options{})
	if err != nil {
		return nil, err
	}
	if final.Skipped < 4 {
		return nil, fmt.Errorf("resume: expected >=4 skipped chunks, got %d", final.Skipped)
	}
	if leftovers, err := countTempFiles(layout.OutDir(root, group)); err != nil {
		return nil, err
	} else if leftovers != 0 {
		return nil, fmt.Errorf("resume: %d temp files remain", leftovers)
	}
	return final, nil
}

func countTempFiles(root string) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if len(name) >= 4 && (endsWith(name, ".tmp") || contains(name, ".tmp-") || contains(name, ".crash.tmp")) {
			n++
		}
		return nil
	})
	return n, err
}

func endsWith(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
