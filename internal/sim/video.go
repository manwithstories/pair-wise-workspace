package sim

import (
	"hash/fnv"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// VideoParams controls deterministic interactive-side frame generation.
type VideoParams struct {
	// Frames is the number of video frames.
	Frames int
	// FPS is the nominal frame rate.
	FPS int
	// VideoSSRC identifies the video stream.
	VideoSSRC uint32
}

// DefaultVideoParams matches a 30fps interactive stream over the simulation.
func DefaultVideoParams() VideoParams {
	return VideoParams{Frames: 180, FPS: 30, VideoSSRC: 0xBADC0DE}
}

// VideoFixture is the generated video stream plus seeded timestamp faults.
type VideoFixture struct {
	Frames []model.VideoFrame
	// MissingPTS lists frame IDs whose timestamp was removed.
	MissingPTS []uint32
	// RegressivePTS lists frame IDs whose timestamp was moved backward.
	RegressivePTS []uint32
}

// BuildVideo generates frames at nominal FPS. The corrupt and burst
// scenarios inject deterministic timestamp faults; nominal and recovery keep
// a clean monotonic timeline.
func BuildVideo(scenario string, seed int64, vp VideoParams) *VideoFixture {
	if vp.Frames <= 0 || vp.FPS <= 0 {
		vp = DefaultVideoParams()
	}
	step := clock.Ticks(int64(clock.TicksPerSecond) / int64(vp.FPS))
	out := make([]model.VideoFrame, vp.Frames)
	for i := 0; i < vp.Frames; i++ {
		pts := step * clock.Ticks(i)
		h := fnv.New64a()
		_, _ = h.Write([]byte{byte(i), byte(i >> 8), byte(seed), byte(seed >> 8)})
		out[i] = model.VideoFrame{
			FrameID:     uint32(i),
			PTS:         pts,
			ContentHash: h.Sum64(),
		}
	}
	fix := &VideoFixture{Frames: out}
	switch scenario {
	case ScenarioCorrupt:
		// Deterministic faults independent of RNG ordering.
		for _, id := range []uint32{12, 47, 99} {
			if int(id) < vp.Frames {
				fix.MissingPTS = append(fix.MissingPTS, id)
				out[id].PTS = 0
			}
		}
		for _, id := range []uint32{30, 88} {
			if int(id)+1 < vp.Frames {
				fix.RegressivePTS = append(fix.RegressivePTS, id)
				out[id].PTS = out[id-1].PTS - step
			}
		}
	case ScenarioBurst:
		for _, id := range []uint32{20} {
			if int(id) < vp.Frames {
				fix.RegressivePTS = append(fix.RegressivePTS, id)
				out[id].PTS = out[id-1].PTS - clock.Milliseconds(1500)
			}
		}
	}
	return fix
}
