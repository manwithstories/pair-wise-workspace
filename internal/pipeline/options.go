package pipeline

// Options controls one transactional job.
type Options struct {
	OutDir           string
	Resume           bool
	CrashAfterMux    bool
	TargetBPS        int64
	QueueItems       int
	QueueBytes       int
	MemoryLimitBytes int64
}
