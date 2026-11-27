package remux

import (
	"context"
	"fmt"
	"time"

	"github.com/asticode/go-astiav"

	"mediacheck/internal/model"
)

// Write muxes one packet.  Timestamps are rescaled from the packet timebase
// to the millisecond Matroska timebase, and the first packet of each track
// defines that track's zero so a fixture that starts at an arbitrary slot
// offset does not produce a huge initial timestamp.
func (w *Writer) Write(ctx context.Context, p model.Packet) error {
	if w.closed || w.committed {
		return fmt.Errorf("remux: writer is closed")
	}
	if err := ctx.Err(); err != nil {
		w.failed = true
		return fmt.Errorf("%w: %v", ErrAborted, err)
	}
	st, ok := w.streams[p.Track]
	if !ok {
		return nil // track not part of this output
	}
	if w.written >= MaxPacketsPerRun {
		return fmt.Errorf("remux: packet limit reached: %w", model.ErrLimitExceeded)
	}
	base, seen := w.basetime[p.Track]
	if !seen {
		base = p.Slot.Start
		w.basetime[p.Track] = base
	}
	startMs := (p.Slot.Start - base) / int64(time.Millisecond)
	durMs := p.Slot.Duration() / int64(time.Millisecond)
	if durMs <= 0 {
		durMs = 1
	}
	// Matroska block timestamps are strictly increasing per track.  A
	// recovered placeholder can land on the same millisecond as the packet
	// that follows it, so clamp rather than emitting an out-of-order block.
	if last, ok := w.lastMs[p.Track]; ok && startMs <= last {
		startMs = last + 1
	}
	w.lastMs[p.Track] = startMs
	data := p.Data
	if p.Track == model.TrackAudio && w.infos[p.Track].Codec == model.CodecAAC {
		// Matroska stores raw AAC payloads; the ADTS framing that carriage
		// uses is expressed by AudioSpecificConfig in the codec private
		// data.  Strip the header rather than muxing a stream that would
		// decode as ADTS inside a raw stream.
		stripped, err := stripADTS(data)
		if err != nil {
			return fmt.Errorf("remux: %s packet %d: %w", p.Track, p.Sequence, err)
		}
		data = stripped
	}
	w.packets.Unref()
	if err := w.packets.FromData(data); err != nil {
		return fmt.Errorf("remux: filling packet: %w", err)
	}
	w.packets.SetStreamIndex(st.Index())
	w.packets.SetPts(startMs)
	w.packets.SetDts(startMs)
	w.packets.SetDuration(durMs)
	w.packets.SetPos(-1)
	flags := []astiav.PacketFlag{}
	if p.Keyframe {
		flags = append(flags, astiav.PacketFlagKey)
	}
	w.packets.SetFlags(astiav.NewPacketFlags(flags...))
	if err := w.ofc.WriteInterleavedFrame(w.packets); err != nil {
		return fmt.Errorf("remux: writing %s packet %d: %w", p.Track, p.Sequence, err)
	}
	w.written++
	w.bytes += int64(len(p.Data))
	return nil
}
