package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"turncode/internal/config"
	"turncode/internal/quality"
	"turncode/internal/rate"
	"turncode/internal/state"
	"turncode/internal/yuv"
)

const metaFile = "run.json"
const bitstreamFile = "bitstream.bin"

type RunMeta struct {
	Version    int         `json:"version"`
	Spec       config.Spec `json:"spec"`
	SpecHash   string      `json:"spec_hash"`
	SourcePath string      `json:"source_path"`
	QPOffset   int         `json:"qp_offset"`
}

func writeMeta(workDir string, meta RunMeta) error {
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return state.AtomicWrite(filepath.Join(workDir, metaFile), append(raw, '\n'), 0o600)
}

func LoadMeta(workDir string) (RunMeta, error) {
	path := filepath.Join(workDir, metaFile)
	if err := rejectLink(path); err != nil {
		return RunMeta{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return RunMeta{}, err
	}
	var meta RunMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return RunMeta{}, fmt.Errorf("pipeline: decode run metadata: %w", err)
	}
	if meta.Version != 1 {
		return RunMeta{}, fmt.Errorf("pipeline: unsupported run metadata version")
	}
	meta.Spec.Defaults()
	if err := meta.Spec.Validate(); err != nil {
		return RunMeta{}, fmt.Errorf("%w: %v", config.ErrInvalidSpec, err)
	}
	if meta.SpecHash != hashSpec(meta.Spec) {
		return RunMeta{}, fmt.Errorf("pipeline: metadata checksum mismatch")
	}
	return meta, nil
}

func hashSpec(spec config.Spec) string {
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LoadJob reconstructs a resumable job from work/run.json.
func LoadJob(workDir string) (*Job, error) {
	meta, err := LoadMeta(workDir)
	if err != nil {
		return nil, err
	}
	return &Job{
		Spec:       meta.Spec,
		SourcePath: meta.SourcePath,
		WorkDir:    workDir,
		QPOffset:   meta.QPOffset,
		Resume:     true,
	}, nil
}

type execution struct {
	ctx         context.Context
	job         *Job
	spec        config.Spec
	meta        RunMeta
	machine     *state.Machine
	states      *state.StateStore
	checkpoints *state.CheckpointStore
	bitstream   *os.File
	source      *os.File
	cp          state.Checkpoint
	controller  *rate.Controller
	prevFrame   *yuv.Frame
}

// RunJob validates, resumes if requested, and processes GOPs serially.
func RunJob(ctx context.Context, job *Job) (Outcome, error) {
	if job == nil {
		return Outcome{}, fmt.Errorf("%w: nil job", ErrInvalidJob)
	}
	out := Outcome{}
	if job.WorkDir == "" || job.SourcePath == "" {
		return out, fmt.Errorf("%w: missing work/source path", ErrInvalidJob)
	}
	job.Spec.Defaults()
	if err := job.Spec.Validate(); err != nil {
		return out, err
	}
	if err := os.MkdirAll(job.WorkDir, 0o755); err != nil {
		return out, err
	}
	meta := RunMeta{Version: 1, Spec: job.Spec, SourcePath: job.SourcePath, QPOffset: job.QPOffset}
	meta.SpecHash = hashSpec(meta.Spec)
	if job.Resume {
		loaded, err := LoadMeta(job.WorkDir)
		if err != nil {
			return out, fmt.Errorf("pipeline: load resumable job: %w", err)
		}
		meta = loaded
		job.Spec, job.SourcePath, job.QPOffset = meta.Spec, meta.SourcePath, meta.QPOffset
	} else {
		if _, err := os.Stat(filepath.Join(job.WorkDir, metaFile)); err == nil {
			return out, ErrWorkConflict
		}
		if err := writeMeta(job.WorkDir, meta); err != nil {
			return out, err
		}
	}
	if err := rejectLink(job.SourcePath); err != nil {
		return out, err
	}
	info, err := os.Stat(job.SourcePath)
	if err != nil {
		return out, err
	}
	if info.Size() != meta.Spec.FileBytes {
		return out, fmt.Errorf("%w: source size=%d manifest=%d", config.ErrInvalidSpec, info.Size(), meta.Spec.FileBytes)
	}
	x, err := newExecution(ctx, job, meta)
	if err != nil {
		return out, err
	}
	defer x.close()
	if err := x.enterEncoding(); err != nil {
		return out, err
	}
	return x.loop()
}

func newExecution(ctx context.Context, job *Job, meta RunMeta) (*execution, error) {
	x := &execution{
		ctx:         ctx,
		job:         job,
		spec:        meta.Spec,
		meta:        meta,
		machine:     state.New(),
		states:      state.NewStateStore(job.WorkDir),
		checkpoints: state.NewCheckpointStore(job.WorkDir),
	}
	bsPath := filepath.Join(job.WorkDir, bitstreamFile)
	if !job.Resume {
		var openErr error
		x.bitstream, openErr = os.OpenFile(bsPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if openErr != nil {
			return nil, fmt.Errorf("%w: bitstream already exists", ErrWorkConflict)
		}
		if openErr != nil {
			return nil, fmt.Errorf("pipeline: create bitstream: %w", openErr)
		}
		x.cp = state.Checkpoint{
			Version:        state.JournalVersion,
			SpecHash:       meta.SpecHash,
			ResumedFromGOP: 0,
			Quality:        *quality.NewAccumulator(meta.Spec.SegmentFrames),
		}
	} else {
		var err error
		if err := rejectLink(bsPath); err != nil {
			return nil, err
		}
		x.bitstream, err = os.OpenFile(bsPath, os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("pipeline: open checkpointed bitstream: %w", err)
		}
		if rec, recovered, err := x.states.Load(); err != nil {
			return nil, fmt.Errorf("pipeline: state journal: %w", err)
		} else {
			if recovered {
				fmt.Fprintln(os.Stderr, "state journal recovered from backup")
			}
			x.machine.Force(rec.State)
		}
		size, _ := x.bitstream.Seek(0, io.SeekEnd)
		rec, err := x.checkpoints.Recover(size, meta.SpecHash)
		if err != nil {
			return nil, fmt.Errorf("pipeline: checkpoint recovery: %w", err)
		}
		if rec.Fallback || rec.Truncate {
			if err := x.bitstream.Truncate(rec.Checkpoint.BitstreamOffset); err != nil {
				return nil, fmt.Errorf("pipeline: rollback bitstream: %w", err)
			}
			if err := x.bitstream.Sync(); err != nil {
				return nil, err
			}
			fmt.Fprintf(os.Stderr, "recovered from GOP %d\n", rec.Checkpoint.CompletedGOPs)
		}
		x.cp = rec.Checkpoint
		if x.cp.ResumedFromGOP == 0 {
			x.cp.ResumedFromGOP = x.cp.CompletedGOPs
		}
		if x.cp.Quality.SegmentFrames != meta.Spec.SegmentFrames {
			return nil, fmt.Errorf("%w: checkpoint segment size mismatch", state.ErrCorruptJournal)
		}
	}
	cfg := rate.Config{
		TargetBPS:  meta.Spec.TargetBPS,
		FPS:        meta.Spec.FPS,
		GOP:        meta.Spec.GOP,
		PixelCount: meta.Spec.Width * meta.Spec.Height,
	}
	controller, err := rate.New(cfg)
	if err != nil {
		return nil, err
	}
	if job.Resume {
		controller.Restore(x.cp.Buffer)
	}
	x.controller = controller
	x.source, err = os.Open(job.SourcePath)
	if err != nil {
		return nil, err
	}
	if _, err := x.bitstream.Seek(x.cp.BitstreamOffset, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := x.source.Seek(int64(x.cp.NextFrame)*x.spec.FrameBytes, io.SeekStart); err != nil {
		return nil, err
	}
	if x.cp.NextFrame > 0 {
		prevPos := int64(x.cp.NextFrame-1) * x.spec.FrameBytes
		if _, err := x.source.Seek(prevPos, io.SeekStart); err == nil {
			x.prevFrame, _ = yuv.ReadFrame(x.source, x.spec.Width, x.spec.Height)
		}
		if _, err := x.source.Seek(int64(x.cp.NextFrame)*x.spec.FrameBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return x, nil
}

func rejectLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pipeline: symlink rejected: %s", path)
	}
	return nil
}

func (x *execution) close() {
	if x.bitstream != nil {
		_ = x.bitstream.Close()
	}
	if x.source != nil {
		_ = x.source.Close()
	}
}

func (x *execution) enterEncoding() error {
	if !x.job.Resume {
		if _, err := x.states.Save(state.Pending); err != nil {
			return err
		}
		if err := x.machine.Transition(state.Validating); err != nil {
			return err
		}
		if _, err := x.states.Save(state.Validating); err != nil {
			return err
		}
		if err := x.machine.Transition(state.Analyzing); err != nil {
			return err
		}
		if _, err := x.states.Save(state.Analyzing); err != nil {
			return err
		}
		if err := x.machine.Transition(state.Encoding); err != nil {
			return err
		}
		_, err := x.states.Save(state.Encoding)
		return err
	} else {
		cur := x.machine.State()
		if cur == state.Encoding {
			if err := x.machine.Transition(state.Paused); err != nil {
				return err
			}
			cur = state.Paused
		}
		if cur == state.Failed {
			if err := x.machine.Transition(state.Pending); err != nil {
				return err
			}
			cur = state.Pending
		}
		if cur == state.Pending {
			if err := x.machine.Transition(state.Validating); err != nil {
				return err
			}
			cur = state.Validating
		}
		if cur == state.Validating {
			if err := x.machine.Transition(state.Analyzing); err != nil {
				return err
			}
			cur = state.Analyzing
		}
		if cur == state.Analyzing {
			if err := x.machine.Transition(state.Encoding); err != nil {
				return err
			}
		} else if cur == state.Paused {
			if err := x.machine.Transition(state.Encoding); err != nil {
				return err
			}
		} else if x.machine.State() != state.Encoding {
			return fmt.Errorf("pipeline: cannot resume from state %s", x.machine.State())
		}
	}
	_, err := x.states.Save(state.Encoding)
	return err
}
