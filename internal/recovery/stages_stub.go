package recovery

// Temporary no-op stage stubs keep the orchestration skeleton compilable while
// individual stages are implemented in small reviewable batches.

func (e *exec) stageCodec() error    { return nil }
func (e *exec) stageResample() error { return nil }
func (e *exec) stageLoudness() error { return nil }
func (e *exec) stageKeyframe() error { return nil }
func (e *exec) stageRTP() error      { return nil }
func (e *exec) stageLedger() error   { return nil }
func (e *exec) stageOutput() error   { return nil }
