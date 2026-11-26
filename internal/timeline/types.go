package timeline

// TrackSpec declares one track's clock and explicit synchronization edges.
type TrackSpec struct {
	ID        uint32
	Timescale uint32
	DependsOn []uint32
}
