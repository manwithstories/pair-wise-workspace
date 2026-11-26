package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mediaqc/internal/dag"
	"mediaqc/internal/jitter"
	"mediaqc/internal/media"
	"mediaqc/internal/metrics"
	"mediaqc/internal/network"
	"mediaqc/internal/recovery"
	rtpio "mediaqc/internal/rtp"
	"mediaqc/internal/sync"
)

// Output contains terminal metrics and the deterministic hashed artifact.
type Output struct {
	Config    Config
	Stats     metrics.Stats
	Artifact  []byte
	DAG       *dag.Report
	TraceJSON []byte
	Malformed []string
	Conflicts []string
}

type handledEvent struct {
	ev     wireEvent
	result rtpio.Result
}

// Run generates samples, executes the media path and schedules metric nodes.
func Run(cfg Config) (*Output, error) {
	if cfg.Profile == "" {
		cfg.Profile = "normal"
	}
	sc, err := generateScenario(cfg)
	if err != nil {
		return nil, err
	}
	out := &Output{Config: cfg}

	ing := rtpio.NewIngestor()
	repairer := recovery.NewRepairer(128)
	var handled []handledEvent
	var firstMalformed error
	var firstConflict error

	var jitterReleased []*rtpio.Packet
	var jitterGaps []jitter.Gap
	var e2eMS []float64
	var repairReport *recovery.Report
	var syncReport *sync.Report

	sched := dag.NewScheduler(dag.NewJournal())
	mustAdd := func(n *dag.Node) {
		if addErr := sched.Add(n); addErr != nil {
			panic(addErr)
		}
	}

	mustAdd(&dag.Node{
		ID:    "sample.generate",
		Input: []byte(fmt.Sprintf("%s|%s|%d", cfg.Profile, cfg.Codec, cfg.Seed)),
		Run: func(context.Context, map[string][]byte) ([]byte, error) {
			b, _ := json.Marshal(sc.Trace)
			out.TraceJSON = b
			return []byte(fmt.Sprintf("samples=%d,%d", sc.Trace.PacketCount, sc.Trace.VideoCount)), nil
		},
	})

	mustAdd(&dag.Node{
		ID:   "rtp.ingest",
		Deps: []string{"sample.generate"},
		Run: func(context.Context, map[string][]byte) ([]byte, error) {
			for _, ev := range sc.Events {
				res := ing.HandleDatagram(ev.Raw, time.Duration(ev.ArrivalMS)*time.Millisecond)
				if res.Err != nil {
					if media.Classify(res.Err) == media.KindMalformed {
						out.Malformed = append(out.Malformed, res.Err.Error())
						if firstMalformed == nil {
							firstMalformed = res.Err
						}
					}
					if media.Classify(res.Err) == media.KindConflict {
						out.Conflicts = append(out.Conflicts, res.Err.Error())
						if firstConflict == nil {
							firstConflict = res.Err
						}
					}
				}
				if res.Verdict == rtpio.VerdictAccepted && res.Packet != nil {
					handled = append(handled, handledEvent{ev: ev, result: res})
					if ev.Retransmission && ev.SSRC == sc.Trace.AudioSSRC {
						if err := repairer.AddRetransmission(res.Packet); err != nil {
							return nil, err
						}
					}
				}
			}
			return []byte(fmt.Sprintf("received=%d accepted=%d dup=%d", ing.Stats.Received, ing.Stats.Accepted, ing.Stats.Duplicates)), nil
		},
	})

	mustAdd(&dag.Node{
		ID:   "jitter.buffer",
		Deps: []string{"rtp.ingest"},
		Run: func(context.Context, map[string][]byte) ([]byte, error) {
			jb := jitter.NewBuffer(sc.Trace.AudioSSRC, sc.StartExtSeq,
				networkWait(sc, 0), 128)
			for _, he := range handled {
				if he.ev.Retransmission || he.ev.SSRC != sc.Trace.AudioSSRC {
					continue
				}
				p := he.result.Packet
				if err := jb.Push(p); err != nil {
					return nil, err
				}
				jb.SetWaitMS(networkWait(sc, int(p.ArrivalMS)))
				released, gaps := jb.Advance(p.ArrivalMS)
				jitterGaps = append(jitterGaps, gaps...)
				jitterReleased = append(jitterReleased, released...)
				collectE2E(sc, released, p.ArrivalMS, &e2eMS)
			}
			last := sc.StartExtSeq + uint64(sc.Trace.PacketCount-1)
			released, tailGaps := jb.FlushThrough(last)
			jitterGaps = append(jitterGaps, tailGaps...)
			jitterReleased = append(jitterReleased, released...)
			for _, p := range released {
				idx := int(p.ExtSequence - sc.StartExtSeq)
				if idx >= 0 && idx < len(sc.SendMS) {
					e2eMS = append(e2eMS, float64(p.ArrivalMS)-float64(sc.SendMS[idx])+5)
				}
			}
			return []byte(fmt.Sprintf("released=%d gaps=%d", len(jitterReleased), len(jitterGaps))), nil
		},
	})

	repairCalls := 0
	mustAdd(&dag.Node{
		ID:          "audio.repair",
		Deps:        []string{"jitter.buffer"},
		MaxAttempts: 2,
		Run: func(context.Context, map[string][]byte) ([]byte, error) {
			repairCalls++
			if cfg.FailOnce == "audio.repair" && repairCalls == 1 {
				return nil, media.New(media.KindInternal, "audio.repair", "injected fail-once")
			}
			repairReport = repairer.Repair(jitterReleased, jitterGaps)
			for _, g := range jitterGaps {
				for _, seq := range g.Missing {
					idx := int(seq - sc.StartExtSeq)
					if idx < 0 || idx >= len(sc.SendMS) || !repairer.HasCopy(seq) {
						continue
					}
					e2eMS = append(e2eMS, float64(g.DetectedMS)-float64(sc.SendMS[idx])+6)
				}
			}
			b, _ := json.Marshal(struct {
				Recovered int `json:"recovered"`
				Gaps      int `json:"gaps"`
			}{repairReport.Recovered, repairReport.UnrecoverableGap})
			return b, nil
		},
	})

	mustAdd(&dag.Node{
		ID:   "sync.align",
		Deps: []string{"audio.repair"},
		Run: func(context.Context, map[string][]byte) ([]byte, error) {
			if repairReport == nil {
				return nil, media.New(media.KindInternal, "sync.align", "repair report missing")
			}
			audioFrames := make([]sync.Frame, 0, len(repairReport.Frames))
			for i, f := range repairReport.Frames {
				audioFrames = append(audioFrames, sync.Frame{
					Stream: "audio", Index: i,
					PTS: uint64(i) * sc.AudioTicks, DurationTicks: sc.AudioTicks, Gap: f.Gap,
				})
			}
			syncReport = sync.Align(audioFrames, sc.VideoFrames)
			return []byte(fmt.Sprintf("p95=%.3f gaps=%d", syncReport.P95AbsMS, syncReport.PropagatedGaps)), nil
		},
	})

	mustAdd(&dag.Node{
		ID:   "metrics.report",
		Deps: []string{"sync.align"},
		Run: func(context.Context, map[string][]byte) ([]byte, error) {
			stats := metrics.Stats{
				Profile: cfg.Profile, Codec: sc.Codec, Seed: cfg.Seed,
				Packets: ing.Stats.Accepted, WirePackets: ing.Stats.Received,
				Duplicates: ing.Stats.Duplicates, Conflicts: ing.Stats.Conflicts, Malformed: ing.Stats.Malformed,
				Recovered: repairReport.Recovered, RecoverableLoss: repairReport.RecoverableLoss,
				UnrecoverableGaps: repairReport.UnrecoverableGap, RecoveryRate: repairReport.RecoveryRate,
				AVSyncP95MS: syncReport.P95AbsMS, E2EP95MS: metrics.Percentile(e2eMS, 0.95),
			}
			avAbs := make([]float64, 0, len(syncReport.Samples))
			for _, s := range syncReport.Samples {
				avAbs = append(avAbs, s.AbsMS)
			}
			artifact := metrics.Artifact{
				SchemaVersion: "mediaqc/v1", Profile: cfg.Profile, Codec: sc.Codec, Seed: cfg.Seed,
				Stats: stats, E2ESamplesMS: e2eMS, AVAbsMS: avAbs,
			}
			hash, raw, hashErr := metrics.HashArtifact(artifact)
			if hashErr != nil {
				return nil, hashErr
			}
			stats.OutputSHA256 = hash
			out.Stats = stats
			out.Artifact = raw
			return raw, nil
		},
	})

	dagReport, dagErr := sched.Run(context.Background())
	out.DAG = dagReport
	if dagReport != nil {
		out.Stats.FirstBlocked = dagReport.FirstBlocked
	}
	if dagErr != nil {
		return out, dagErr
	}
	if firstMalformed != nil {
		return out, firstMalformed
	}
	if firstConflict != nil {
		return out, firstConflict
	}
	return out, nil
}

func networkWait(sc *scenario, nowMS int) int {
	return network.WaitWindowMS(sc.Trace.ConditionAt(nowMS))
}

func collectE2E(sc *scenario, released []*rtpio.Packet, nowMS int64, samples *[]float64) {
	for _, p := range released {
		idx := int(p.ExtSequence - sc.StartExtSeq)
		if idx < 0 || idx >= len(sc.SendMS) {
			continue
		}
		latency := float64(nowMS) - float64(sc.SendMS[idx]) + 3
		if latency < 0 {
			latency = 0
		}
		*samples = append(*samples, latency)
	}
}
