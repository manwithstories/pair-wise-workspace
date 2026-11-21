package timeline

// Gap is a transport gap expressed on the timeline in timeline ticks. It is
// converted from transport.Gap by the CLI so the timeline package does not
// depend on transport internals.
type Gap struct {
	Start int64
	End   int64
	// Blocked marks a burst gap that blocks dependent frames.
	Blocked bool
	// Dep names the first blocked dependency associated with the gap.
	Dep string
}
