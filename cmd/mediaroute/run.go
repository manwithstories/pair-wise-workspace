package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediaroute/internal/checkpoint"
	"mediaroute/internal/index"
	"mediaroute/internal/packet"
	"mediaroute/internal/provenance"
	"mediaroute/internal/repair"
)

// sha256File digests a file for input/output verification.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// execParams drives one recovery run (used by run and demo).
type execParams struct {
	inputPath, outPath, ckPath string
	cfg                        repair.Config
	interruptAfter             int
	useCheckpoint              bool
}

// execResult summarizes a finished or interrupted run.
type execResult struct {
	report      repair.Report
	recovered   bool
	resumeMs    int64
	outputSHA   string
	interrupted bool
}

// execute runs the recovery pipeline over the input, optionally
// resuming from / writing a checkpoint, and writes the manifest next
// to the output file.
func execute(pp execParams) (*execResult, int, error) {
	inputSHA, err := sha256File(pp.inputPath)
	if err != nil {
		return nil, exitUsage, err
	}
	cfgSHA := pp.cfg.SHA256()

	var cp *checkpoint.Checkpoint
	if pp.useCheckpoint {
		cp, err = checkpoint.Load(pp.ckPath)
		if err != nil {
			return nil, exitUsage, err // corrupt checkpoint: refuse reuse
		}
		if cp != nil {
			if err := cp.Validate(inputSHA, cfgSHA); err != nil {
				return nil, exitUsage, err
			}
		}
	}
	recovered := cp != nil

	in, err := os.Open(pp.inputPath)
	if err != nil {
		return nil, exitUsage, err
	}
	defer in.Close()
	var out *os.File
	if recovered {
		out, err = os.OpenFile(pp.outPath, os.O_RDWR, 0o600)
		if err != nil {
			return nil, exitUsage, fmt.Errorf("checkpoint exists but output: %w", err)
		}
		if err := out.Truncate(cp.OutputCommitted); err != nil {
			return nil, exitUsage, err
		}
		if _, err := out.Seek(0, io.SeekEnd); err != nil {
			return nil, exitUsage, err
		}
	} else {
		out, err = os.OpenFile(pp.outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, exitUsage, err
		}
	}

	start := time.Now()
	rd := packet.NewReader(in)
	var pipe *repair.Pipeline
	skip := 0
	if recovered {
		skip = cp.RecordsProcessed
	}
	res := &execResult{recovered: recovered}
	for {
		rec, rerr := rd.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return nil, exitUsage, rerr
		}
		if rec.Session != nil && pipe == nil {
			var idxw *index.Writer
			metas, err := repair.TrackMetas(rec.Session)
			if err != nil {
				out.Close()
				return nil, exitMedia, err
			}
			idxw = index.NewWriter(out, metas)
			pipe, err = repair.NewPipeline(pp.cfg, rec.Session, idxw)
			if err != nil {
				out.Close()
				return nil, exitMedia, err
			}
			if recovered {
				var st repair.State
				if err := json.Unmarshal(cp.Pipeline, &st); err != nil {
					out.Close()
					return nil, exitUsage, fmt.Errorf("checkpoint corrupt: %w", err)
				}
				if err := pipe.Import(&st, cp.OutputCommitted); err != nil {
					out.Close()
					return nil, exitUsage, err
				}
			} else if err := idxw.WriteInit(); err != nil {
				out.Close()
				return nil, exitUsage, err
			}
		}
		if rec.Index < skip {
			continue
		}
		if pipe == nil {
			out.Close()
			return nil, exitUsage, fmt.Errorf("record %d: packet before session description", rec.Index)
		}
		if err := pipe.Process(rec); err != nil {
			out.Close()
			return nil, exitUsage, err
		}
		if pp.interruptAfter > 0 && pipe.Records() >= pp.interruptAfter {
			st, err := pipe.Export()
			if err != nil {
				out.Close()
				return nil, exitUsage, err
			}
			raw, err := json.Marshal(st)
			if err != nil {
				out.Close()
				return nil, exitUsage, err
			}
			ck := &checkpoint.Checkpoint{
				InputSHA256: inputSHA, ConfigSHA256: cfgSHA,
				RecordsProcessed: pipe.Records(), OutputCommitted: pipe.OutputOffset(),
				Pipeline: raw,
			}
			if err := checkpoint.Save(pp.ckPath, ck); err != nil {
				out.Close()
				return nil, exitUsage, err
			}
			out.Sync()
			out.Close()
			res.interrupted = true
			res.report = pipe.Report()
			return res, exitInterrupt, nil
		}
	}
	if pipe == nil {
		out.Close()
		return nil, exitUsage, errors.New("input has no session record")
	}
	if err := pipe.Finish(); err != nil {
		out.Close()
		return nil, exitMedia, err
	}
	out.Sync()
	out.Close()

	sha, err := sha256File(pp.outPath)
	if err != nil {
		return nil, exitUsage, err
	}
	man := pipe.Manifest()
	man.InputSHA256 = inputSHA
	man.ConfigSHA256 = cfgSHA
	man.Params["window"] = uint64(pp.cfg.Window)
	man.Params["drift_ppm"] = uint64(pp.cfg.DriftPPM)
	man.OutputSHA256 = sha
	man.TimeBase = pipe.TimeBases()
	man.Recovered = recovered
	if recovered {
		man.ResumeMs = time.Since(start).Milliseconds()
		res.resumeMs = man.ResumeMs
	}
	manPath := filepath.Join(filepath.Dir(pp.outPath), "manifest.json")
	if err := provenance.Write(manPath, man); err != nil {
		return nil, exitUsage, err
	}
	if pp.useCheckpoint {
		// The run completed: the checkpoint is consumed and must not
		// be reused for a later run.
		if err := os.Remove(pp.ckPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, exitUsage, err
		}
	}
	res.report = pipe.Report()
	res.outputSHA = sha
	return res, exitOK, nil
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	input := fs.String("input", "", "input RTP capture file")
	ck := fs.String("checkpoint", "", "checkpoint file")
	out := fs.String("out", "", "output MP4 file")
	interrupt := fs.Int("interrupt-after", 0, "stop and checkpoint after N records")
	window := fs.Uint("window", 64, "reorder window in packets")
	drift := fs.Uint("drift-ppm", 1000, "clock drift threshold in ppm")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if *input == "" || *ck == "" || *out == "" {
		return exitUsage, errors.New("run requires --input, --checkpoint and --out")
	}
	inPath, err := resolveInput(*input)
	if err != nil {
		return exitUsage, err
	}
	outPath, err := resolveOutput(*out)
	if err != nil {
		return exitUsage, err
	}
	ckPath, err := resolveOutput(*ck)
	if err != nil {
		return exitUsage, err
	}
	res, code, err := execute(execParams{
		inputPath: inPath, outPath: outPath, ckPath: ckPath,
		cfg:            repair.Config{Window: uint32(*window), DriftPPM: uint32(*drift)},
		interruptAfter: *interrupt,
		useCheckpoint:  true,
	})
	if err != nil {
		return code, err
	}
	if res.interrupted {
		fmt.Printf("checkpoint_packets=%d\n", res.report.Records)
		return exitInterrupt, nil
	}
	fmt.Printf("recovered=%v resume_ms=%d sha256=%s\n", res.recovered, res.resumeMs, res.outputSHA)
	return exitOK, nil
}
