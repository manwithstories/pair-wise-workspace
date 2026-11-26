package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"

	"pairflow/internal/ingest"
	"pairflow/internal/model"
	"pairflow/internal/transport"
)

// Config controls deterministic sample generation and the proof run.
type Config struct {
	WorkDir           string
	Seed              int64
	Tracks            int
	FramesPerTrack    int
	PayloadBytes      int
	DriftPPM          float64
	Rollover          uint32
	VFRSeed           int64
	DropSeqs          []int
	DamageTrack       string
	DamageFrame       int64
	CorruptByte       int
	CrashAfterRecords int
}

// Validate classifies bad command-line parameters with exit code 64.
func (c Config) Validate() error {
	invalid := func(msg string) error {
		return &model.MediaError{Code: model.CodeInvalidArgument, Message: msg}
	}
	if c.Tracks <= 0 || c.Tracks > 64 {
		return invalid("tracks must be between 1 and 64")
	}
	if c.FramesPerTrack <= 0 || c.FramesPerTrack > 100000 {
		return invalid("frames-per-track out of range")
	}
	if c.PayloadBytes <= 0 || c.PayloadBytes > 1<<20 {
		return invalid("payload-bytes out of range")
	}
	if c.CorruptByte < 0 || c.CorruptByte >= c.PayloadBytes {
		return invalid("corrupt-byte must lie inside payload")
	}
	if c.DamageFrame < 0 || c.DamageFrame >= int64(c.FramesPerTrack) {
		return invalid("damage-frame out of range")
	}
	if c.CrashAfterRecords < 0 || c.CrashAfterRecords > c.Tracks*c.FramesPerTrack {
		return invalid("crash-after-records out of range")
	}
	if c.DamageTrack == "" {
		return invalid("damage-track is required")
	}
	return nil
}

// GeneratedFrame owns one frame and its payload.
type GeneratedFrame struct {
	Frame       model.Frame
	Payload     []byte
	ReferenceNS int64
}

// Dataset is the complete locally generated proof corpus.
type Dataset struct {
	Config            Config
	SourceFingerprint string
	Tracks            []model.Track
	DurationsNS       []int64
	Frames            []*GeneratedFrame
	Audio             model.AudioTrack
}

// Generate builds deterministic VFR video, drifted RTP timestamps and PCM.
func Generate(cfg Config) (*Dataset, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ds := &Dataset{Config: cfg}

	vfr := rand.New(rand.NewSource(cfg.VFRSeed))
	intervalChoices := []int64{16_666_667, 20_000_000, 33_333_333, 40_000_000, 50_000_000}
	ds.DurationsNS = make([]int64, cfg.FramesPerTrack)
	pts := int64(0)
	for i := range ds.DurationsNS {
		ds.DurationsNS[i] = intervalChoices[vfr.Intn(len(intervalChoices))]
		pts += ds.DurationsNS[i]
	}
	totalNS := pts

	canonical, _ := json.Marshal(struct {
		Seed    int64   `json:"seed"`
		Tracks  int     `json:"tracks"`
		Frames  int     `json:"frames"`
		Payload int     `json:"payload"`
		Drift   float64 `json:"drift_ppm"`
		Roll    uint32  `json:"rollover"`
		VFR     int64   `json:"vfr_seed"`
	}{cfg.Seed, cfg.Tracks, cfg.FramesPerTrack, cfg.PayloadBytes, cfg.DriftPPM, cfg.Rollover, cfg.VFRSeed})
	fpSum := sha256.Sum256(canonical)
	ds.SourceFingerprint = hex.EncodeToString(fpSum[:])

	for ti := 0; ti < cfg.Tracks; ti++ {
		trackID := fmt.Sprintf("cam-%02d", ti+1)
		track := model.Track{
			ID:          trackID,
			Index:       ti + 1,
			Kind:        model.KindVideo,
			SSRC:        transport.DefaultSSRC + uint32(ti),
			ClockRateHz: 90000,
			StartRTP:    cfg.Rollover,
			DriftPPM:    cfg.DriftPPM,
		}
		ds.Tracks = append(ds.Tracks, track)

		for fi := 0; fi < cfg.FramesPerTrack; fi++ {
			truePTS := int64(0)
			for d := 0; d < fi; d++ {
				truePTS += ds.DurationsNS[d]
			}
			driftFactor := 1 + cfg.DriftPPM/1_000_000
			ticks := int64(float64(truePTS) * float64(track.ClockRateHz) / float64(model.TimebaseHz) * driftFactor)
			rtpTS := uint32(uint64(cfg.Rollover) + uint64(ticks))

			rng := rand.New(rand.NewSource(cfg.Seed + int64(ti+1)*1_000_003 + int64(fi)*37))
			payload := make([]byte, cfg.PayloadBytes)
			_, _ = rng.Read(payload)
			parity, mask := ingest.NewLocator(payload)
			color := model.ColorMetadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited"}.Normalize()
			meta := model.Frame{
				TrackID:      trackID,
				TrackIndex:   ti + 1,
				Sequence:     int64(fi),
				SourceOffset: int64(fi * cfg.PayloadBytes),
				CapturedAtNS: truePTS,
				RTPTimestamp: rtpTS,
				PTSNS:        truePTS,
				DurationNS:   ds.DurationsNS[fi],
				Color:        color,
				PayloadBytes: len(payload),
				SHA256:       ingest.Sum256(payload),
				LocatorXOR:   parity,
				LocatorMask:  mask,
			}
			ds.Frames = append(ds.Frames, &GeneratedFrame{Frame: meta, Payload: payload, ReferenceNS: truePTS})
		}
	}

	ds.Audio = generateAudio(cfg.Seed, totalNS)
	return ds, nil
}

func generateAudio(seed int64, durationNS int64) model.AudioTrack {
	samples := int((durationNS*int64(model.AudioSampleRateHz) + model.TimebaseHz - 1) / model.TimebaseHz)
	pcm := make([]byte, samples*2)
	rng := rand.New(rand.NewSource(seed ^ 0xA0D10))
	for i := 0; i < samples; i++ {
		// Quiet deterministic pseudo-sine-like PCM; alignment uses its grid.
		v := int16(((i*17)%997 - 498) * 4)
		if rng.Intn(100) == 0 {
			v = int16(rng.Intn(200) - 100)
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return model.AudioTrack{
		ID:         "commentary-01",
		SampleRate: model.AudioSampleRateHz,
		Channels:   1,
		PTSNS:      0,
		PCM:        pcm,
		SHA256:     ingest.Sum256(pcm),
	}
}
