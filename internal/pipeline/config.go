// Package pipeline wires RTP ingest, clock calibration, the bounded queue,
// audio conversion/PLC/loudness and the transactional WAL into one
// deterministic processing run, including injectable faults and replay.
package pipeline

import (
	"context"

	"mediamix/internal/audio"
	"mediamix/internal/buffer"
	"mediamix/internal/rtp"
)

// Fault names recognised by the runner. A fault aborts the current
// transaction; the runner rolls back to the last commit and reprocesses.
type Fault string

const (
	FaultNone          Fault = ""
	FaultAfterNormalize Fault = "after-normalize" // after resample, before loudness
	FaultBeforeCommit  Fault = "before-commit"    // after loudness, before WAL commit
)

// Config controls one pipeline run.
type Config struct {
	WorkDir         string
	QueueBytes      int
	QueueItems      int
	SlowConsumerMs  int
	Fault           Fault
	Source          rtp.StreamConfig
	Reorder         rtp.ReorderConfig
	Target          audio.TargetSpec
}

// DefaultConfig returns safe local defaults within the 8 MiB queue cap.
func DefaultConfig(workDir string) Config {
	return Config{
		WorkDir:        workDir,
		QueueBytes:     buffer.MaxQueueBytes,
		QueueItems:     4096,
		SlowConsumerMs: 0,
		Fault:          FaultNone,
		Source:         rtp.DefaultStreamConfig(),
		Reorder:        rtp.DefaultReorderConfig(),
		Target:         audio.DefaultTarget(),
	}
}

// Frame is the unit that travels through the queue: an ordered RTP packet
// annotated with its shared-clock tick.
type Frame struct {
	Packet   *rtp.Packet
	Tick     int64
	Seq      uint16
	Payload  []byte
}

// ByteSize implements the queue sizer so the byte budget covers payloads.
func (f Frame) ByteSize() int { return len(f.Payload) }

// runState holds the per-run accumulators shared by the stages.
type runState struct {
	ctx      context.Context
	cancel   context.CancelFunc
	queue    *buffer.Window[Frame]
	plc      *audio.PLC
	conv     *audio.Converter
	loud     *audio.LoudnessNormaliser
}
