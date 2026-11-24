package source

// Spec parameterizes a self-generated demo stream.
type Spec struct {
	Case               string
	SampleRate         int
	Channels           int
	FramesPerPacket    int
	PacketCount        int
	SSRC               uint32
	PayloadType        uint8
	ReorderWindow      int
	IntervalSamples    int
	RetentionIntervals int

	// Damaged-case orchestration.
	DuplicateSeq   int
	SwapA          int
	SwapB          int
	DroppedSeqs    []int
	EarlyLateSeq   int
	LateSeq        int
	ExpiredLateSeq int
}

// CaseSpec returns the named deterministic fixture specification.
func CaseSpec(name string) Spec {
	switch name {
	case "damaged":
		return Spec{
			Case:               "damaged",
			SampleRate:         48000,
			Channels:           2,
			FramesPerPacket:    480,
			PacketCount:        100,
			SSRC:               0x4d454432,
			PayloadType:        96,
			ReorderWindow:      8,
			IntervalSamples:    4800,
			RetentionIntervals: 2,
			DuplicateSeq:       5,
			SwapA:              10,
			SwapB:              11,
			DroppedSeqs:        []int{20, 21, 22},
			EarlyLateSeq:       40,
			LateSeq:            40,
			ExpiredLateSeq:     70,
		}
	case "recovery":
		return Spec{
			Case:               "recovery",
			SampleRate:         44100,
			Channels:           2,
			FramesPerPacket:    441,
			PacketCount:        100,
			SSRC:               0x4d454433,
			PayloadType:        96,
			ReorderWindow:      16,
			IntervalSamples:    4800,
			RetentionIntervals: 10,
			DuplicateSeq:       0,
		}
	default: // clean
		return Spec{
			Case:               "clean",
			SampleRate:         44100,
			Channels:           1,
			FramesPerPacket:    441,
			PacketCount:        100,
			SSRC:               0x4d454431,
			PayloadType:        96,
			ReorderWindow:      16,
			IntervalSamples:    4800,
			RetentionIntervals: 10,
			DuplicateSeq:       -1,
		}
	}
}
