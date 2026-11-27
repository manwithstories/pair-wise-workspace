package pipeline

// RunOptions configures one batch job.
type RunOptions struct {
	Seed   int64
	Faults []string
	State  string
	Out    string
	Resume bool // reserved: checkpoint identity already implies resume
	IDRs   int
	GOP    int
	Audio  int
}

// Report is the terminal-facing result of a demo run.
type Report struct {
	Source          string
	Seed            int64
	Faults          []string
	RTPPackets      int
	TSBytes         int
	Recovered       int
	Unrecoverable   int
	Roundtrip       float64
	KeyframeHitRate float64
	Segments        int
	Digests         []string
	ContentDigests  []string
	Gaps            []GapInterval
	InputSHA        string
	CacheHits       int
	CacheMisses     int
	CheckpointSaved int
	ResumedSegments int
	Fatal           bool
	Notes           []string
}
