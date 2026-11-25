package transport

// seqLess compares sequence numbers with wrap-around semantics.
func seqLess(a, b uint32) bool { return int32(a-b) < 0 }

// SeqDiff returns a-b as a signed distance (wrap-safe).
func SeqDiff(a, b uint32) int32 { return int32(a - b) }
