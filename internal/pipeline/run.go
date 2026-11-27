package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"relayctl/internal/dag"
)

// CLI exit code contract.
const (
	ExitOK        = 0
	ExitDamaged   = 2
	ExitLost      = 3
	ExitUsage     = 4
	ExitPaused    = 75
)

// Run executes the graph once, honoring a configured pause point. It returns
// the populated status; the exit code is carried on Status.ExitCode.
func (e *Engine) Run() (Status, error) {
	start := time.Now()
	runner := dag.NewRunner(e.journal, e.cfg.PauseAfter)
	res, err := runner.Run(e.buildGraph())
	elapsed := time.Since(start)

	st := e.collectStatus(res, elapsed)
	// The DAG records node errors as text rather than aborting; recover the
	// originating sentinel so the CLI maps the correct exit code.
	rootErr := e.rootError(res, err)
	var pauseErr *dag.PauseError
	switch {
	case errors.As(err, &pauseErr):
		st.ExitCode = ExitPaused
		return st, err
	case errors.Is(rootErr, ErrDamaged):
		st.ExitCode = ExitDamaged
		st.FirstBlocked = firstBlockName(res)
		return st, rootErr
	case errors.Is(rootErr, ErrUnrecoverable):
		st.ExitCode = ExitLost
		st.FirstBlocked = firstBlockName(res)
		return st, rootErr
	case errors.Is(rootErr, ErrBlocked):
		st.ExitCode = ExitDamaged
		st.FirstBlocked = firstBlockName(res)
		return st, rootErr
	case rootErr != nil:
		st.ExitCode = ExitLost
		return st, rootErr
	}
	st.ExitCode = ExitOK
	return st, nil
}

// rootError picks the earliest node failure and re-hydrates its sentinel.
func (e *Engine) rootError(res dag.RunResult, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if len(res.Failures) == 0 {
		return nil
	}
	// Deterministic choice: lowest node name in declaration order.
	for _, name := range []string{NodeIngest, NodeRecover, NodeParse, NodeSync, NodePack, NodeVerify} {
		text, ok := res.Failures[name]
		if !ok {
			continue
		}
		switch {
		case strings.Contains(text, ErrUnrecoverable.Error()):
			return fmt.Errorf("%w: %s", ErrUnrecoverable, text)
		case strings.Contains(text, ErrDamaged.Error()):
			return fmt.Errorf("%w: %s", ErrDamaged, text)
		case strings.Contains(text, ErrBlocked.Error()):
			return fmt.Errorf("%w: %s", ErrBlocked, text)
		default:
			return errors.New(text)
		}
	}
	return nil
}

// Resume continues a paused/failed run without a pause point; the journal
// ensures only unfinished nodes execute.
func (e *Engine) Resume() (Status, error) {
	e.cfg.PauseAfter = ""
	return e.Run()
}

// Verify rebuilds the manifest repeatedly and confirms every repeat yields
// the same digest, populating RepeatEqual. It runs the graph first (idempotent
// via the journal) so it works both standalone and after a paused resume.
func (e *Engine) Verify(repeats int) (Status, error) {
	if repeats < 1 {
		repeats = 1
	}
	if st, err := e.Run(); err != nil {
		return st, err
	}
	out := e.outputDir()
	first, err := buildDigest(out)
	if err != nil {
		return Status{ExitCode: ExitLost}, err
	}
	equal := true
	for i := 1; i < repeats; i++ {
		next, err := buildDigest(out)
		if err != nil {
			return Status{ExitCode: ExitLost}, err
		}
		if next != first {
			equal = false
		}
	}
	st, _ := e.Run()
	st.SHA256 = first
	st.RepeatEqual = equal
	st.ExitCode = ExitOK
	return st, nil
}

func (e *Engine) outputDir() string { return e.cfg.StateDir + "/out" }

func firstBlockName(res dag.RunResult) string {
	if res.FirstBlock != nil {
		return res.FirstBlock.Node
	}
	return ""
}

// collectStatus maps internal node state into the CLI-visible status shape.
func (e *Engine) collectStatus(res dag.RunResult, elapsed time.Duration) Status {
	st := Status{
		Recovered:      e.recovery.Recovered,
		Unrecoverable: len(e.recovery.Unrecoverable),
		SyncOffsetMS:   e.syncResult.WorstAbsMS,
		SyncConfidence: e.syncResult.Confidence.String(),
		ResumeMS:       elapsed.Milliseconds(),
		SHA256:         e.manifestDigest,
		FirstBlocked:   firstBlockName(res),
	}
	if e.damageOff != 0 || e.cfg.Damaged {
		st.DamageFound = e.damageOff
		st.DamageExpected = e.scenario.Manifest.ExpectedDamageOffset
		st.DamageDelta = e.damageOff - e.scenario.Manifest.ExpectedDamageOffset
	}
	return st
}
