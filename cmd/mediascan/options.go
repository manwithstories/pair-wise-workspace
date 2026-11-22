// Command mediascan is the terminal entry point for deterministic media
// stream quality analysis. It generates frame and RTP fixtures, validates
// the manifest and MP4 container, reorders the RTP trace, evaluates the
// bitrate controller and commits chunked results to a local index with
// resumable checkpoints.
package main

// Exit codes form the stable CLI contract.
const (
	exitOK           = 0 // completed and input valid
	exitCorruptInput = 2 // input or manifest defect
	exitIndexFault   = 3 // index/checkpoint inconsistency
	exitInterrupted  = 4 // recoverable interruption
)

// Options is the parsed demo subcommand configuration.
type Options struct {
	Seed           int64
	Frames         int
	FPS            int
	GOP            int
	TargetKbps     int
	ChunkFrames    int
	BandwidthTrace string
	Out            string
	Corrupt        []string
	InterruptAfter int // chunk count after which to simulate an interrupt
	Resume         bool
	RebuildIndex   bool
	ReorderWindow  uint16
}

func defaultOptions() Options {
	return Options{
		Seed: 73, Frames: 900, FPS: 30, GOP: 60, TargetKbps: 900,
		ChunkFrames: 120, BandwidthTrace: "built-in", Out: "./tmp",
		InterruptAfter: -1, ReorderWindow: 64,
	}
}
