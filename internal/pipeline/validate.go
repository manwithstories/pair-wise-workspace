package pipeline

import (
	"avpipe/internal/audio"
	"avpipe/internal/fault"
	"avpipe/internal/stream"
)

// ValidateOptions rejects impossible combinations with an input-class error so
// the CLI can exit 2 before doing any work.
func ValidateOptions(o Options, layout, srcLayout string, queueMs float64) error {
	if _, err := audio.ParseLayout(layout); err != nil {
		return err
	}
	if _, err := audio.ParseLayout(srcLayout); err != nil {
		return err
	}
	if _, err := stream.ParsePolicy(o.Policy); err != nil {
		return err
	}
	switch {
	case o.Loss < 0 || o.Loss >= 1:
		return fault.New(fault.CodeInvalidArgument, "cli.loss",
			"--loss %v must be in [0,1)", o.Loss)
	case o.JitterMs < 0:
		return fault.New(fault.CodeInvalidArgument, "cli.jitter",
			"--jitter-ms %v must be >= 0", o.JitterMs)
	case o.ConsumerDelayMs < 0:
		return fault.New(fault.CodeInvalidArgument, "cli.consumer-delay",
			"--consumer-delay-ms %v must be >= 0", o.ConsumerDelayMs)
	case o.QueueBytes <= 0:
		return fault.New(fault.CodeInvalidArgument, "cli.queue-bytes",
			"--queue-bytes %d must be positive", o.QueueBytes)
	case queueMs <= 0:
		return fault.New(fault.CodeInvalidArgument, "cli.queue-ms",
			"--queue-ms %v must be positive", queueMs)
	case o.TargetRate < 4000 || o.TargetRate > 384000:
		return fault.New(fault.CodeInvalidArgument, "cli.sample-rate",
			"--sample-rate %d outside 4000..384000", o.TargetRate)
	case o.SourceRate < 4000 || o.SourceRate > 384000:
		return fault.New(fault.CodeInvalidArgument, "cli.source-sample-rate",
			"--source-sample-rate %d outside 4000..384000", o.SourceRate)
	case o.BlockSize <= 0:
		return fault.New(fault.CodeInvalidArgument, "cli.block-size",
			"--block-size %d must be positive", o.BlockSize)
	case o.Frames < 0:
		return fault.New(fault.CodeInvalidArgument, "cli.frames",
			"--frames %d must be >= 0", o.Frames)
	case o.RecoveryWindow <= 0:
		return fault.New(fault.CodeInvalidArgument, "cli.recovery-window",
			"--recovery-window %d must be positive", o.RecoveryWindow)
	case o.Frames > 0 && o.Frames < 2*o.BlockSize:
		// Two blocks are the minimum for a meaningful RTP/alignment run.
		return fault.New(fault.CodeInvalidArgument, "cli.frames",
			"--frames %d must cover at least two blocks of %d frames", o.Frames, o.BlockSize)
	}
	return audio.ValidateLoudnessConfig(audio.LoudnessConfig{
		TargetLUFS: o.TargetLUFS, PeakCeiling: o.PeakCeiling})
}
