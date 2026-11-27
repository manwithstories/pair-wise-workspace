package fixture

import (
	"math/rand"

	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

// Generate creates a deterministic fixture stream and its RTP view.
func Generate(opts Options) (*Stream, error) {
	if opts.IDRs <= 0 {
		opts.IDRs = 8
	}
	if opts.GOP <= 0 {
		opts.GOP = 4
	}
	if opts.AudioPerGOP <= 0 {
		opts.AudioPerGOP = 3
	}
	if opts.PayloadSize <= 0 || opts.PayloadSize > ts.PacketSize*7 || opts.PayloadSize%ts.PacketSize != 0 {
		opts.PayloadSize = ts.PacketSize
	}
	b := &builder{opts: opts, rng: rand.New(rand.NewSource(opts.Seed)), cc: make(map[uint16]uint8)}
	b.writePAT(0)
	b.writePMT(0)
	faults := map[string]bool{}
	for _, f := range opts.Faults {
		faults[f] = true
	}
	videoStart := int64(clock.TicksPerSecond) * 10
	if faults[FaultPCRWrap] {
		// Start half a second before the 33-bit PCR/PTS wrap so the stamped
		// clock crosses it naturally during the run.
		videoStart = int64(clock.Wrap)*300 - int64(clock.TicksPerSecond)/2
	}
	vidia := 0
	audia := 0
	for g := 0; g < opts.IDRs; g++ {
		for f := 0; f < opts.GOP; f++ {
			key := f == 0
			b.pts = videoStart + int64(vidia)*(clock.TicksPerSecond/25)
			var nal byte = 0x01
			if key {
				nal = 0x05
			}
			payload := make([]byte, 12+opts.PayloadSize)
			payload[0], payload[1], payload[2], payload[3] = 0, 0, 0, 1
			payload[4] = nal
			for i := 12; i < len(payload); i++ {
				payload[i] = byte(b.rng.Intn(251))
			}
			pcr := int64(-1)
			if f == 0 {
				pcr = b.pts
			}
			ccJump := faults[FaultCCJump] && vidia == 2
			b.writeVideo(b.pts, pcr, key, payload, ccJump)
			vidia++
			if f == 0 || f == opts.GOP-1 {
				for a := 0; a < opts.AudioPerGOP; a++ {
					apts := b.pts + int64(a)*(clock.TicksPerSecond/6)
					b.writeAudio(apts)
					audia++
				}
			}
		}
	}
	b.writePAT(1)
	b.writePMT(1)
	if faults[FaultBadPSI] {
		b.injectBadPSI()
	}
	raw := b.out
	if faults[FaultTruncate] && len(raw) > ts.PacketSize*3 {
		raw = raw[:len(raw)-ts.PacketSize/2]
	}
	st := &Stream{TS: raw, Faults: append([]string(nil), opts.Faults...)}
	st.Packets, st.Groups = b.toRTP(st.TS, opts.PayloadSize, faults)
	return st, nil
}
