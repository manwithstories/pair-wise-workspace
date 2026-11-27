package pipeline

import (
	"sort"
	"time"

	"avflow/internal/fixture"
	"avflow/internal/model"
)

// Precision evaluates localization accuracy: the distance, in packets, between
// the first fault the pipeline reported and the nearest defect the fixture
// actually injected. A clean stream expects zero faults and zero false alarms.
func Precision(g *fixture.Generated, first *model.Fault) int {
	if first == nil {
		return 0
	}
	best := -1
	for _, injectedSeq := range g.FaultSeqs {
		d := int64(first.PacketSeq) - int64(injectedSeq)
		if d < 0 {
			d = -d
		}
		if best == -1 || int(d) < best {
			best = int(d)
		}
	}
	if best == -1 {
		return 0
	}
	return best
}

// FalsePositives counts reported faults that do not correspond to any injected
// defect. On a clean fixture every reported fault is a false positive. Faults
// that are direct downstream symptoms of an injected defect (e.g. boundary
// drift caused by an injected sample-rate conflict) are not false alarms.
func FalsePositives(g *fixture.Generated, reported []*model.Fault) int {
	injected := map[model.FaultType]bool{}
	for ft := range g.FaultSeqs {
		injected[ft] = true
	}
	fp := 0
	for _, f := range reported {
		if injected[f.Type] {
			continue
		}
		if f.Type == model.FaultBoundaryDrift && injected[model.FaultRateConflict] {
			continue // expected symptom of the injected rate conflict
		}
		fp++
	}
	return fp
}

// P95 measures the 95th-percentile per-packet processing window. Each sample
// is one full pipeline invocation over a bounded chunk; callers pass the
// measured durations so the timing harness stays separate from the core.
func P95(durations []time.Duration) float64 {
	if len(durations) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), durations...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (len(cp) * 95) / 100
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	// Nanosecond precision keeps sub-millisecond windows visible instead of
	// truncating a fast run to zero.
	return float64(cp[idx].Nanoseconds()) / 1_000_000.0
}

// BenchmarkRun executes the pipeline repeatedly and returns the p95 window in
// milliseconds over the given number of iterations on the same work root.
func BenchmarkRun(data []byte, workDir string, iterations int) (float64, error) {
	durations := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		dir := workDir + "/bench-" + itoa(i)
		start := time.Now()
		if _, _, err := Run(data, Config{WorkDir: dir}); err != nil {
			// Recoverable faults still count as a completed window.
			if f, ok := model.AsFault(err); ok && f.Recoverable {
				durations = append(durations, time.Since(start))
				continue
			}
			return 0, err
		}
		durations = append(durations, time.Since(start))
	}
	return P95(durations), nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
