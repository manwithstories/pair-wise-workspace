// Package analysis runs content-level analytics on decoded media: per-channel
// silence detection with a local noise gate, and content fingerprints used to
// de-duplicate across cameras and HLS/DASH representations. Fingerprints are
// computed from decoded content (not container bytes) so re-encoded near
// matches can be recognised without collapsing genuinely different clips.
package analysis

import (
	"fmt"

	"github.com/asticode/go-astiav"
)

// decodedStream holds an open decoder bound to one input stream.
type decodedStream struct {
	fmtCtx   *astiav.FormatContext
	codecCtx *astiav.CodecContext
	stream   *astiav.Stream
	index    int
}

// openSegment opens a segment read-only and prepares decoders for its audio
// and video streams. The returned closer frees all CGo resources.
func openSegment(path string) (fmtCtx *astiav.FormatContext, streams map[astiav.MediaType]*decodedStream, closer func(), err error) {
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, nil, nil, fmt.Errorf("analysis: alloc format context")
	}
	cleanupFns := []func(){fc.Free}
	success := false
	defer func() {
		if !success {
			for i := len(cleanupFns) - 1; i >= 0; i-- {
				cleanupFns[i]()
			}
		}
	}()

	if err = fc.OpenInput(path, nil, nil); err != nil {
		return nil, nil, nil, fmt.Errorf("analysis: open %s: %w", path, err)
	}
	cleanupFns = append(cleanupFns, fc.CloseInput)
	if err = fc.FindStreamInfo(nil); err != nil {
		return nil, nil, nil, fmt.Errorf("analysis: stream info %s: %w", path, err)
	}

	streams = map[astiav.MediaType]*decodedStream{}
	for _, st := range fc.Streams() {
		mt := st.CodecParameters().MediaType()
		if mt != astiav.MediaTypeAudio && mt != astiav.MediaTypeVideo {
			continue
		}
		if _, exists := streams[mt]; exists {
			continue // first stream of each type only
		}
		codec := astiav.FindDecoder(st.CodecParameters().CodecID())
		if codec == nil {
			continue
		}
		cc := astiav.AllocCodecContext(codec)
		if cc == nil {
			return nil, nil, nil, fmt.Errorf("analysis: alloc codec ctx")
		}
		cleanupFns = append(cleanupFns, cc.Free)
		if err = st.CodecParameters().ToCodecContext(cc); err != nil {
			return nil, nil, nil, fmt.Errorf("analysis: codec params: %w", err)
		}
		if err = cc.Open(codec, nil); err != nil {
			return nil, nil, nil, fmt.Errorf("analysis: open decoder: %w", err)
		}
		cc.SetTimeBase(st.TimeBase())
		streams[mt] = &decodedStream{
			fmtCtx: fc, codecCtx: cc, stream: st, index: st.Index(),
		}
	}

	closer = func() {
		for i := len(cleanupFns) - 1; i >= 0; i-- {
			cleanupFns[i]()
		}
	}
	success = true
	return fc, streams, closer, nil
}

// decodeFrames reads every packet for the given media type and invokes handle
// for each decoded frame. The frame is only valid for the duration of handle.
func decodeFrames(streams map[astiav.MediaType]*decodedStream, mt astiav.MediaType, handle func(*astiav.Frame) error) error {
	ds, ok := streams[mt]
	if !ok {
		return nil
	}
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()

	for {
		err := ds.fmtCtx.ReadFrame(pkt)
		if err != nil {
			if err == astiav.ErrEof {
				break
			}
			return err
		}
		if pkt.StreamIndex() != ds.index {
			pkt.Unref()
			continue
		}
		if err := ds.codecCtx.SendPacket(pkt); err != nil {
			pkt.Unref()
			// A truncated tail can surface here; surface it to the caller.
			return fmt.Errorf("analysis: send packet: %w", err)
		}
		pkt.Unref()
		for {
			recvErr := ds.codecCtx.ReceiveFrame(frame)
			if recvErr != nil {
				if recvErr == astiav.ErrEagain || recvErr == astiav.ErrEof {
					break
				}
				return fmt.Errorf("analysis: receive frame: %w", recvErr)
			}
			if err := handle(frame); err != nil {
				return err
			}
			frame.Unref()
		}
	}
	// Flush the decoder.
	if err := ds.codecCtx.SendPacket(nil); err != nil && err != astiav.ErrEof {
		// Flush errors on truncated media are non-fatal for analytics.
	}
	for {
		recvErr := ds.codecCtx.ReceiveFrame(frame)
		if recvErr != nil {
			break
		}
		if err := handle(frame); err != nil {
			return err
		}
		frame.Unref()
	}
	return nil
}
