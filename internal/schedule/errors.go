package schedule

// dagError describes a graph-construction problem (unknown dep, cycle, etc.).
type dagError struct{ msg string }

func (e *dagError) Error() string { return "schedule: " + e.msg }

func errDAG(msg string) error { return &dagError{msg: msg} }
