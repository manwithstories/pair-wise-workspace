package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
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
	kind := fs.String("kind", "truncated-box", "corruption kind (truncated-box, corrupt-sample-table, tail-truncation, corrupt-sample, unknown-required-box, zero-timescale)")
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
	if fixture.Kind(*kind) == fixture.KindJournalMismatch {
		return corruptJournal(*dir)
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
	if readErr == nil && fixture.Kind(*kind) == fixture.KindPayloadFlip {
		// A flipped payload byte is structurally valid, so integrity is only
		// detectable by comparing sample digests against a known-good run.
		readErr = verifyPayloadDrift(context.Background(), *dir, damagedPath, dmg)
	}
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
