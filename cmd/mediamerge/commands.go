package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/txn"
	"mediamerge/internal/verify"
)

// runCorrupt damages the generated input in place and reports the precise
// byte locator produced by validation. The exit code stays 0 when the locator
// matches within one byte, which keeps the acceptance chain scriptable.
func runCorrupt(args []string) error {
	fs := newFlagSet("corrupt")
	dir := fs.String("dir", "", "work directory")
	kind := fs.String("kind", "truncated-box", "corruption kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireDir(fs, *dir); err != nil {
		return err
	}
	input := filepath.Join(*dir, "input.mp4")
	damagedPath := filepath.Join(*dir, "input."+*kind+".mp4")
	data, err := os.ReadFile(input)
	if err != nil {
		return model.WrapError(model.StageVerify, model.CodeIntegrity, 0, 0, false, err,
			"read input for corruption")
	}
	damaged, dmg, err := fixture.Corrupt(data, fixture.Kind(*kind))
	if err != nil {
		return err
	}
	// The damaged bytes are a separate fixture: the clean input stays intact
	// so recovery can still re-derive the mapping afterwards.
	if err := os.WriteFile(damagedPath, damaged, 0o644); err != nil {
		return fmt.Errorf("corrupt: write damaged fixture: %w", err)
	}
	_, readErr := demux.Read(context.Background(), damagedPath)
	if readErr == nil {
		fmt.Printf("error_offset=-1 locator_delta_bytes=-1\n")
		return model.NewError(model.StageVerify, model.CodeIntegrity, -1, 0, false,
			"corruption kind %q was not detected", *kind)
	}
	var pe *model.Error
	if !errors.As(readErr, &pe) {
		return readErr
	}
	delta := int64(-1)
	if dmg.LocatorTarget >= 0 {
		delta = pe.Offset - dmg.LocatorTarget
		if delta < 0 {
			delta = -delta
		}
	}
	fmt.Printf("error_offset=%d locator_delta_bytes=%d\n", pe.Offset, delta)
	if dmg.LocatorTarget >= 0 && delta > 1 {
		return model.NewError(model.StageVerify, model.CodeIntegrity, pe.Offset, 0, false,
			"locator delta %d exceeds one byte (expected %d)", delta, dmg.LocatorTarget)
	}
	return nil
}

// runRecover tests the journaled recovery path. With --interrupt-after it
// deliberately crashes after the given stage, then recovers and resumes.
func runRecover(args []string) error {
	fs := newFlagSet("recover")
	dir := fs.String("dir", "", "work directory")
	interruptAfter := fs.Int("interrupt-after", 2, "commit this stage then simulate a crash (0 disables)")
	queue := fs.Int("queue", 4, "bounded sample queue capacity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireDir(fs, *dir); err != nil {
		return err
	}
	ctx := context.Background()
	if err := verify.EnsureDir(*dir); err != nil {
		return err
	}
	input := filepath.Join(*dir, "input.mp4")
	base, err := readInputPlan(ctx, input)
	if err != nil {
		return err
	}
	if *interruptAfter > 0 {
		// Run until the requested stage, leaving a durable journal and a
		// partial (deliberately uncommitted) tail.
		if err := crashAfter(ctx, *dir, base, *interruptAfter, *queue); err != nil {
			return err
		}
	}
	start := nowMS()
	res, err := txn.Resume(ctx, *dir, base.reader, base.plan, *queue)
	if err != nil {
		return err
	}
	elapsed := nowMS() - start
	fmt.Printf("recovered_stage=%d resume_ms=%d rollback_bytes=%d output_sha256=%s\n",
		res.RecoveredStage, elapsed, res.RollbackBytes, res.OutputSHA)
	return nil
}

// runMerge remuxes an arbitrary input file with explicit mapping rules.
func runMerge(args []string) error {
	fs := newFlagSet("merge")
	input := fs.String("input", "", "input MP4/MOV file")
	output := fs.String("output", "", "target MP4 file")
	mapSpec := fs.String("map", "", `mapping rules, e.g. "1=10:video,2=11:audio"`)
	queue := fs.Int("queue", 8, "bounded sample queue capacity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		fs.Usage()
		return model.NewError(model.StageVerify, model.CodePath, -1, 0, false,
			"--input and --output are required")
	}
	ctx := context.Background()
	r, err := demux.Open(ctx, *input)
	if err != nil {
		return err
	}
	defer r.Close()
	rules, err := mapping.ParseRules(*mapSpec)
	if err != nil {
		return err
	}
	plan, err := mapping.Apply(r.Tracks, rules)
	if err != nil {
		return err
	}
	res, err := txn.Merge(ctx, filepath.Dir(*output), *output, r, plan, txn.Options{Queue: *queue, InterruptAfter: -1})
	if err != nil {
		return err
	}
	fmt.Println(res.FormatSummary())
	return nil
}

// runPlan prints the validated mapping without writing media.
func runPlan(args []string) error {
	fs := newFlagSet("plan")
	input := fs.String("input", "", "input MP4/MOV file")
	mapSpec := fs.String("map", "", "mapping rules")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		fs.Usage()
		return model.NewError(model.StageVerify, model.CodePath, -1, 0, false, "--input is required")
	}
	r, err := demux.Open(context.Background(), *input)
	if err != nil {
		return err
	}
	defer r.Close()
	rules, err := mapping.ParseRules(*mapSpec)
	if err != nil {
		return err
	}
	plan, err := mapping.Apply(r.Tracks, rules)
	if err != nil {
		return err
	}
	for _, t := range plan.Tracks {
		fmt.Printf("track input=%d target=%d media=%s codec=%s timescale=%d samples=%d\n",
			t.InputID, t.TargetID, t.MediaType, t.Codec, t.Timescale, len(t.Samples))
	}
	return nil
}

type inputPlan struct {
	reader *demux.Reader
	plan   *mapping.Plan
}

func readInputPlan(ctx context.Context, input string) (*inputPlan, error) {
	r, err := demux.Open(ctx, input)
	if err != nil {
		return nil, err
	}
	plan, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		r.Close()
		return nil, err
	}
	return &inputPlan{reader: r, plan: plan}, nil
}

// crashAfter runs the pipeline and stops after the requested stage, leaving a
// journal plus an intentionally uncommitted tail on disk.
func crashAfter(ctx context.Context, dir string, base *inputPlan, stage, queue int) error {
	_, err := txn.Run(ctx, dir, base.reader, base.plan, txn.Options{
		Queue: queue, InterruptAfter: stage, CrashBytes: -1,
	})
	if errors.Is(err, txn.ErrInterrupted) {
		return nil
	}
	if err != nil {
		return err
	}
	return model.NewError(model.StageRecovery, model.CodeIntegrity, -1, 0, false,
		"interrupt-after=%d did not interrupt the run", stage)
}

func nowMS() int64 { return time.Now().UnixNano() / 1e6 }
