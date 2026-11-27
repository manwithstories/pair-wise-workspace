// Package recover fills audio slots that were lost or destroyed in transit.
//
// Three capabilities are tried in order and each outcome is recorded with
// its provenance, because the caller must be able to tell a listener what
// was reconstructed and what was merely filled:
//
//  1. Opus in-band FEC: when the packet after the hole carries LBRR
//     redundancy for the missing slot, libopus decodes it.  This is the only
//     path that reconstructs an approximation of the original signal.
//  2. Opus PLC: libopus extrapolates from decoder state.  It produces
//     plausible audio but is not a reconstruction of the missing samples.
//  3. Silence: a valid, quiet Opus frame keeps the timeline intact when the
//     decoder cannot help at all.
//
// The reported recovery rate counts successfully filled slots (FEC, PLC or
// silence).  It never claims that the original waveform was restored.
package recover

import (
	"fmt"

	"mediacheck/internal/model"
	"mediacheck/internal/opus"
)

// AudioFiller fills missing Opus audio slots.  It keeps a libopus decoder
// whose state follows the real stream, so both FEC and PLC see the right
// history.  A single AudioFiller must only be used from one goroutine.
type AudioFiller struct {
	dec     *opus.Decoder
	enc     *opus.Encoder
	prevLen int // samples per channel of the previous real packet
	preSkip int
}

// NewAudioFiller creates a filler for 48 kHz stereo Opus.  It reports which
// capabilities are actually available rather than assuming them.
func NewAudioFiller() (*AudioFiller, Capabilities, error) {
	f := &AudioFiller{}
	caps := Capabilities{Silence: true}
	dec, err := opus.NewDecoder(48000, 2)
	if err != nil {
		return f, caps, fmt.Errorf("recover: creating Opus decoder: %w", err)
	}
	f.dec = dec
	caps.PLC = true
	enc, err := opus.NewEncoder(48000, 2, 32000, opus.ApplicationVoIP, false, 0)
	if err != nil {
		dec.Close()
		return f, caps, fmt.Errorf("recover: creating Opus encoder: %w", err)
	}
	f.enc = enc
	caps.FEC = true
	return f, caps, nil
}

// Close releases the libopus objects.
func (f *AudioFiller) Close() {
	if f.dec != nil {
		f.dec.Close()
		f.dec = nil
	}
	if f.enc != nil {
		f.enc.Close()
		f.enc = nil
	}
}

// Observe feeds a real audio packet to the decoder state.  samples is the
// packet duration in samples per channel, used for PLC frame sizes.
func (f *AudioFiller) Observe(data []byte, samples int) error {
	if f.dec == nil {
		return fmt.Errorf("recover: filler closed")
	}
	if _, err := f.dec.Decode(data, samples, false); err != nil {
		return fmt.Errorf("recover: decoding real packet: %w", err)
	}
	f.prevLen = samples
	return nil
}

// Fill fills one missing slot of the given duration in samples per channel.
// next is the packet that follows the hole, or nil when the hole is at the
// end of the stream.  It tries FEC, then PLC, then silence and never returns
// an empty packet unless every path failed.
func (f *AudioFiller) Fill(slot model.Slot, samples int, next []byte) SlotResult {
	res := SlotResult{Slot: slot, Method: MethodGiveUp}
	if samples <= 0 {
		samples = f.prevLen
	}
	if samples <= 0 {
		samples = 960
	}
	if f.dec != nil && len(next) > 0 {
		// libopus decodes the in-band redundancy carried by the following
		// packet for the missing slot.  A failed attempt leaves the decoder
		// state untouched, so no re-priming is needed; the next real packet
		// is decoded normally when it arrives.
		if pcm, err := f.dec.Decode(next, samples, true); err == nil && len(pcm) > 0 {
			if data, encErr := f.encodePCM(pcm, samples); encErr == nil {
				f.prevLen = samples
				return SlotResult{Slot: slot, Method: MethodFEC, Data: data, Reason: "in-band FEC"}
			}
		}
	}
	if f.dec != nil {
		if pcm, err := f.dec.Decode(nil, samples, false); err == nil && len(pcm) > 0 {
			if data, encErr := f.encodePCM(pcm, samples); encErr == nil {
				f.prevLen = samples
				return SlotResult{Slot: slot, Method: MethodPLC, Data: data, Reason: "packet loss concealment"}
			}
		}
	}
	if data, err := f.encodeSilence(samples); err == nil {
		f.prevLen = samples
		return SlotResult{Slot: slot, Method: MethodSilence, Data: data, Reason: "silence placeholder"}
	}
	return res
}

func (f *AudioFiller) encodePCM(pcm []int16, samples int) ([]byte, error) {
	if f.enc == nil {
		return nil, fmt.Errorf("recover: encoder closed")
	}
	data, err := f.enc.Encode(pcm, samples)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), data...), nil
}

func (f *AudioFiller) encodeSilence(samples int) ([]byte, error) {
	if f.enc == nil {
		return nil, fmt.Errorf("recover: encoder closed")
	}
	data, err := f.enc.Encode(opusSilence(samples), samples)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), data...), nil
}

func opusSilence(samples int) []int16 { return make([]int16, samples*2) }
