package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediasim/internal/meta"
	"mediasim/internal/pipe"
	"mediasim/internal/txn"
)

type config struct {
	seed          int64
	loss          float64
	burst         int
	queueBytes    int
	consumerDelay time.Duration
	faultSet      string
}

type groupResult struct {
	avSyncP95Ms     float64
	lossRecoverRate float64
	pipelineP95Ms   float64
	peakBufferBytes int
	rollbackCount   int
	digest          string
	duplicates      int
	late            int
	recovered       int
	recoverable     int
	unrecoverable   int
	corrections     int
	marked          int
	committed       int
	aborted         int
	metaErrors      int
}

func parseFlags() (*config, error) {
	cfg := &config{}
	flag.Int64Var(&cfg.seed, "seed", 42, "PRNG seed for synthetic inputs")
	flag.Float64Var(&cfg.loss, "loss", 0.03, "independent packet loss probability [0,0.5]")
	flag.IntVar(&cfg.burst, "burst", 7, "burst loss length in packets")
	flag.IntVar(&cfg.queueBytes, "queue-bytes", 65536, "pipeline queue byte budget")
	flag.DurationVar(&cfg.consumerDelay, "consumer-delay", 8*time.Millisecond, "per-segment consumer delay")
	flag.StringVar(&cfg.faultSet, "fault-set", "all", "fault set: all|normal|corrupt|restart")
	flag.Parse()
	if cfg.loss < 0 || cfg.loss > 0.5 {
		return nil, fmt.Errorf("loss %.3f out of range [0,0.5]", cfg.loss)
	}
	if cfg.burst < 0 {
		return nil, fmt.Errorf("burst %d must be >= 0", cfg.burst)
	}
	if cfg.queueBytes < 1024 {
		return nil, fmt.Errorf("queue-bytes %d must be >= 1024", cfg.queueBytes)
	}
	if cfg.consumerDelay < 0 {
		return nil, fmt.Errorf("consumer-delay must be >= 0")
	}
	switch cfg.faultSet {
	case "all", "normal", "corrupt", "restart":
	default:
		return nil, fmt.Errorf("unknown fault-set %q", cfg.faultSet)
	}
	return cfg, nil
}

func openCommitter(path string) (*txn.WAL, *txn.Committer, error) {
	w, recs, torn, err := txn.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return w, txn.NewCommitter(w, recs, torn), nil
}

// runNormalGroup executes the full pipeline through the bounded queue
// and slow consumer.
func runNormalGroup(cfg *config, dir string) (*groupResult, []txn.Segment, error) {
	data, err := buildPipeline(cfg)
	if err != nil {
		return nil, nil, err
	}
	w, c, err := openCommitter(filepath.Join(dir, "normal.wal"))
	if err != nil {
		return nil, nil, err
	}
	defer w.Close()
	runner := pipe.NewRunner(cfg.queueBytes, cfg.consumerDelay, 2*time.Second)
	stats := runner.Run(data.segments, c)
	var lat []float64
	for _, d := range stats.Latencies {
		lat = append(lat, float64(d.Microseconds())/1000.0)
	}
	res := &groupResult{
		avSyncP95Ms:     p95Ms(data.avSyncMs),
		lossRecoverRate: lossRecoveryRate(data),
		pipelineP95Ms:   p95Ms(lat),
		peakBufferBytes: stats.PeakBytes,
		rollbackCount:   c.RollbackCount(),
		digest:          c.Digest(),
		duplicates:      data.duplicates,
		late:            data.late,
		recovered:       data.recovered,
		recoverable:     data.recoverable,
		unrecoverable:   data.unrecoverable,
		corrections:     data.corrections,
		marked:          data.marked,
		committed:       len(c.Committed()),
		aborted:         stats.Aborted,
	}
	return res, data.segments, nil
}

// runCorruptGroup feeds corrupted metadata and a slow consumer; failed
// validation and backpressure aborts must roll back, never commit.
func runCorruptGroup(cfg *config, dir string, segments []txn.Segment) (*groupResult, error) {
	res := &groupResult{}
	// Corrupted (truncated) emsg segment must be detected at parse time.
	_, emsgEvents := genEvents()
	segBytes, err := meta.BuildSegment(emsgEvents)
	if err != nil {
		return nil, err
	}
	if _, err := meta.ParseSegment(segBytes[:len(segBytes)-7]); err != nil {
		res.metaErrors++
	}

	w, c, err := openCommitter(filepath.Join(dir, "corrupt.wal"))
	if err != nil {
		return nil, err
	}
	defer w.Close()
	// A segment whose payload was corrupted after digesting fails
	// validation and rolls back.
	if len(segments) > 0 {
		bad := segments[0]
		bad.Payload = append([]byte(nil), bad.Payload...)
		bad.Payload[0] ^= 0xFF
		if err := c.Submit(bad); err != nil {
			res.metaErrors++
		}
	}
	// Deterministic slow-consumer emulation over a tiny queue: pushes
	// that cannot proceed abort the current transaction.
	budget := 2 * (len(segments[0].Payload) + 64)
	q := pipe.NewQueue(budget)
	for i, seg := range segments {
		it := pipe.Item{Segment: seg, Size: len(seg.Payload) + 64}
		if err := q.Push(it, 0); err == pipe.ErrBackpressureTimeout {
			c.Abort(seg.ID)
			res.aborted++
		}
		if i%3 == 2 {
			if it, ok := q.Pop(); ok {
				_ = c.Submit(it.Segment)
			}
		}
	}
	q.Close()
	for {
		it, ok := q.Pop()
		if !ok {
			break
		}
		_ = c.Submit(it.Segment)
	}
	res.rollbackCount = c.RollbackCount()
	res.digest = c.Digest()
	res.committed = len(c.Committed())
	res.peakBufferBytes = q.Peak()
	return res, nil
}

// runRestartGroup commits a prefix, simulates a crash that tears the WAL
// tail, then recovers: the uncommitted tail rolls back and processing
// resumes from the last valid commit point.
func runRestartGroup(cfg *config, dir string, segments []txn.Segment) (*groupResult, error) {
	path := filepath.Join(dir, "restart.wal")
	w, c, err := openCommitter(path)
	if err != nil {
		return nil, err
	}
	k := len(segments) / 2
	for _, seg := range segments[:k] {
		if err := c.Submit(seg); err != nil {
			return nil, err
		}
	}
	w.Close()
	// Crash mid-write: a torn record tail follows the last commit.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write([]byte{0x4D, 0x53, 0x57, 0x4C, 0x01, 0x00, 0x00}); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	// Restart: torn tail is discarded, then the remainder commits.
	w2, c2, err := openCommitter(path)
	if err != nil {
		return nil, err
	}
	defer w2.Close()
	res := &groupResult{}
	for _, seg := range segments[k:] {
		if err := c2.Submit(seg); err != nil {
			return nil, err
		}
	}
	res.rollbackCount = c2.RollbackCount()
	res.digest = c2.Digest()
	res.committed = len(c2.Committed())
	return res, nil
}

func printGroup(name string, r *groupResult) {
	fmt.Printf("group=%s\n", name)
	fmt.Printf("av_sync_p95_ms=%.2f\n", r.avSyncP95Ms)
	fmt.Printf("loss_recovery_rate=%.3f\n", r.lossRecoverRate)
	fmt.Printf("pipeline_p95_ms=%.2f\n", r.pipelineP95Ms)
	fmt.Printf("peak_buffer_bytes=%d\n", r.peakBufferBytes)
	fmt.Printf("rollback_count=%d\n", r.rollbackCount)
	fmt.Printf("committed_digest=%s\n", r.digest)
	fmt.Printf("detail duplicates=%d late=%d recovered=%d/%d unrecoverable=%d corrections=%d marked=%d committed=%d aborted=%d meta_errors=%d\n",
		r.duplicates, r.late, r.recovered, r.recoverable, r.unrecoverable,
		r.corrections, r.marked, r.committed, r.aborted, r.metaErrors)
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error=%v\n", err)
		os.Exit(2)
	}
	dir, err := os.MkdirTemp("", "mediasim")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error=%v\n", err)
		os.Exit(2)
	}
	defer os.RemoveAll(dir)

	normal, segments, err := runNormalGroup(cfg, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error=%v\n", err)
		os.Exit(2)
	}
	// Reproducibility: a second run with identical parameters must
	// produce the identical digest.
	normal2, _, err := runNormalGroup(cfg, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error=%v\n", err)
		os.Exit(2)
	}
	printGroup("normal", normal)

	var corrupt, restart *groupResult
	if cfg.faultSet == "all" || cfg.faultSet == "corrupt" {
		corrupt, err = runCorruptGroup(cfg, dir, segments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error=%v\n", err)
			os.Exit(2)
		}
		printGroup("corrupt", corrupt)
	}
	if cfg.faultSet == "all" || cfg.faultSet == "restart" {
		restart, err = runRestartGroup(cfg, dir, segments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error=%v\n", err)
			os.Exit(2)
		}
		printGroup("restart", restart)
	}

	var failures []string
	check := func(ok bool, msg string) {
		if !ok {
			failures = append(failures, msg)
		}
	}
	check(normal.avSyncP95Ms <= 20, "av_sync_p95_ms > 20")
	check(normal.lossRecoverRate >= 0.90, "loss_recovery_rate < 0.90")
	check(normal.pipelineP95Ms <= 250, "pipeline_p95_ms > 250")
	check(normal.peakBufferBytes <= cfg.queueBytes, "peak_buffer_bytes exceeds queue budget")
	check(normal.digest == normal2.digest, "committed_digest not reproducible")
	if corrupt != nil {
		check(corrupt.rollbackCount >= 1, "corrupt rollback_count < 1")
		check(corrupt.metaErrors >= 1, "corrupt metadata not detected")
	}
	if restart != nil {
		check(restart.rollbackCount >= 1, "restart rollback_count < 1")
		check(restart.digest == normal.digest, "restart digest diverges from normal")
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		for _, f := range failures {
			fmt.Printf("threshold_fail=%s\n", f)
		}
		fmt.Println("result=FAIL")
		os.Exit(1)
	}
	fmt.Println("result=PASS")
}
