package fixture

import "fmt"

// Fixture is one generated test input plus metadata tests assert on.
type Fixture struct {
	Name        string
	Data        []byte
	Tracks      []uint64
	// ClusterCount is the number of clusters serialized.
	ClusterCount int
	// BlockCount is the total SimpleBlock count across clusters.
	BlockCount int
	// Truncated marks inputs deliberately cut short.
	Truncated bool
	// Wrap marks inputs exercising uint16 block timecode wrap.
	Wrap bool
	// Late marks inputs whose block order differs from time order.
	Late bool
}

func payload(n int, b byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = b
	}
	return p
}

// Valid builds a healthy three-track (video/audio/subtitle) stream with
// regular 500ms cluster spacing. Block counts per cluster are chosen so
// the demo material yields >=20 aligned segments downstream.
func Valid() *Fixture {
	b := NewBuilder().
		AddTrack(TrackSpec{1, 1, "V_VP8"}).
		AddTrack(TrackSpec{2, 2, "A_OPUS"}).
		AddTrack(TrackSpec{3, 17, "S_TEXT/WEBVTT"})
	blocks := 0
	for c := 0; c < 12; c++ {
		tc := uint64(c * 500)
		b.AddCluster(tc,
			BlockSpec{1, 0, true, payload(32, byte(c))},
			BlockSpec{2, 10, false, payload(16, byte(0x80+c))},
			BlockSpec{1, 100, false, payload(32, byte(c+1))},
			BlockSpec{3, 200, false, payload(8, byte(0x40+c))},
		)
		blocks += 4
	}
	return &Fixture{
		Name: "valid", Data: b.Build(), Tracks: []uint64{1, 2, 3},
		ClusterCount: 12, BlockCount: blocks,
	}
}

// MultiTrackOutOfOrder interleaves tracks inside each cluster with
// non-monotonic relative timecodes (late events).
func MultiTrackOutOfOrder() *Fixture {
	b := NewBuilder().
		AddTrack(TrackSpec{1, 1, "V_VP8"}).
		AddTrack(TrackSpec{2, 2, "A_OPUS"})
	blocks := 0
	for c := 0; c < 8; c++ {
		tc := uint64(c * 500)
		// Audio at +300 arrives before video at +100 (late video).
		b.AddCluster(tc,
			BlockSpec{2, 300, false, payload(12, 0xA0)},
			BlockSpec{1, 100, true, payload(24, 0xB0)},
			BlockSpec{2, -50, false, payload(12, 0xC0)},
			BlockSpec{1, 250, false, payload(24, 0xD0)},
		)
		blocks += 4
	}
	return &Fixture{
		Name: "multitrack_out_of_order", Data: b.Build(), Tracks: []uint64{1, 2},
		ClusterCount: 8, BlockCount: blocks, Late: true,
	}
}

// UnknownLengthTruncated uses an unknown-length Segment and clusters,
// then cuts the tail inside the last cluster's block payload. Because
// every scope is unknown-length, no declared boundary can reject the
// cut early: the parser must localize the truncation while reading the
// final block body.
func UnknownLengthTruncated() *Fixture {
	b := NewBuilder().
		UnknownSegment().
		AddTrack(TrackSpec{1, 1, "V_VP8"}).
		AddTrack(TrackSpec{2, 2, "A_OPUS"})
	for c := 0; c < 6; c++ {
		b.AddUnknownCluster(uint64(c*500),
			BlockSpec{1, 0, true, payload(40, byte(c))},
			BlockSpec{2, 200, false, payload(20, byte(c))},
		)
	}
	raw := b.TruncateTail(5).Build()
	return &Fixture{
		Name: "unknown_length_truncated", Data: raw, Tracks: []uint64{1, 2},
		ClusterCount: 6, BlockCount: 12, Truncated: true,
	}
}

// TimestampWrap places cluster timecodes so that the cluster base plus
// a negative relative timecode crosses the uint16 modular boundary.
func TimestampWrap() *Fixture {
	b := NewBuilder().
		AddTrack(TrackSpec{1, 1, "V_VP8"}).
		AddTrack(TrackSpec{2, 2, "A_OPUS"})
	// Cluster at 65500ms: block at -100 wraps below zero in raw uint16;
	// the next cluster at 66000ms wraps above 65535.
	b.AddCluster(65500,
		BlockSpec{1, -100, true, payload(24, 0x01)},
		BlockSpec{2, 200, false, payload(12, 0x02)},
	)
	b.AddCluster(66000,
		BlockSpec{1, 0, true, payload(24, 0x03)},
		BlockSpec{2, 100, false, payload(12, 0x04)},
	)
	b.AddCluster(66500,
		BlockSpec{1, 0, true, payload(24, 0x05)},
	)
	return &Fixture{
		Name: "timestamp_wrap", Data: b.Build(), Tracks: []uint64{1, 2},
		ClusterCount: 3, BlockCount: 5, Wrap: true,
	}
}

// LateEvents extends the out-of-order pattern with whole blocks whose
// absolute time lands before an already-seen cluster watermark.
func LateEvents() *Fixture {
	b := NewBuilder().
		AddTrack(TrackSpec{1, 1, "V_VP8"}).
		AddTrack(TrackSpec{2, 2, "A_OPUS"})
	b.AddCluster(0,
		BlockSpec{1, 0, true, payload(20, 0x11)},
		BlockSpec{2, 50, false, payload(10, 0x22)},
	)
	b.AddCluster(500,
		// Late video block belonging to the previous window.
		BlockSpec{1, -400, false, payload(20, 0x33)},
		BlockSpec{1, 0, true, payload(20, 0x44)},
		BlockSpec{2, 100, false, payload(10, 0x55)},
	)
	b.AddCluster(1000,
		BlockSpec{1, 0, true, payload(20, 0x66)},
	)
	return &Fixture{
		Name: "late_events", Data: b.Build(), Tracks: []uint64{1, 2},
		ClusterCount: 3, BlockCount: 6, Late: true,
	}
}

// SlowConsumer produces many small blocks so a bounded queue fills
// deterministically under a tiny byte budget.
func SlowConsumer() *Fixture {
	b := NewBuilder().
		AddTrack(TrackSpec{1, 1, "V_VP8"}).
		AddTrack(TrackSpec{2, 2, "A_OPUS"})
	n := 0
	for c := 0; c < 10; c++ {
		var bl []BlockSpec
		for k := 0; k < 6; k++ {
			bl = append(bl,
				BlockSpec{1, int16(k * 40), k == 0, payload(48, byte(n))},
				BlockSpec{2, int16(k*40 + 20), false, payload(24, byte(n))},
			)
			n += 2
		}
		b.AddCluster(uint64(c*500), bl...)
	}
	return &Fixture{
		Name: "slow_consumer", Data: b.Build(), Tracks: []uint64{1, 2},
		ClusterCount: 10, BlockCount: n,
	}
}

// All returns every fixture keyed by Name.
func All() map[string]*Fixture {
	list := []*Fixture{
		Valid(),
		MultiTrackOutOfOrder(),
		UnknownLengthTruncated(),
		TimestampWrap(),
		LateEvents(),
		SlowConsumer(),
	}
	m := make(map[string]*Fixture, len(list))
	for _, f := range list {
		m[f.Name] = f
	}
	return m
}

// Describe renders a one-line human summary for terminal output.
func (f *Fixture) Describe() string {
	return fmt.Sprintf("%-26s bytes=%-6d clusters=%d blocks=%d tracks=%v flags=%s%s%s",
		f.Name, len(f.Data), f.ClusterCount, f.BlockCount, f.Tracks,
		flag(f.Truncated, "T"), flag(f.Wrap, "W"), flag(f.Late, "L"))
}

func flag(on bool, mark string) string {
	if on {
		return mark
	}
	return "-"
}
