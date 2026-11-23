package model

import (
	"encoding/binary"
	"fmt"
)

// Payload framing. Every payload this project writes starts with a four-byte
// codec tag so a later stage can prove the frame is still decodable even when
// the record checksum is intact (a damaged recorder can produce that).
var (
	VideoPayloadTag = [4]byte{'M', 'P', 'V', '1'}
	AudioPayloadTag = [4]byte{'M', 'P', 'A', '1'}
)

const minPayloadBytes = 4

// EncodeVideoPayload builds a video payload carrying declared dimensions and
// a frame index followed by opaque content.
func EncodeVideoPayload(width, height uint16, index uint32, content []byte) []byte {
	out := make([]byte, 4+2+2+4+len(content))
	copy(out[:4], VideoPayloadTag[:])
	binary.BigEndian.PutUint16(out[4:], width)
	binary.BigEndian.PutUint16(out[6:], height)
	binary.BigEndian.PutUint32(out[8:], index)
	copy(out[12:], content)
	return out
}

// EncodeAudioPayload builds an audio payload carrying a frame index and block
// count. Energy values themselves live in Frame.Energy, so they are not
// duplicated into the payload.
func EncodeAudioPayload(index uint32, blocks uint16, content []byte) []byte {
	out := make([]byte, 4+4+2+len(content))
	copy(out[:4], AudioPayloadTag[:])
	binary.BigEndian.PutUint32(out[4:], index)
	binary.BigEndian.PutUint16(out[8:], blocks)
	copy(out[10:], content)
	return out
}

// ValidatePayload checks the media framing of a frame belonging to a track of
// the given kind. A failure is reported as corruption with the frame's track,
// sequence number and original byte offset so an operator can find it.
func ValidatePayload(kind Kind, fr *Frame) error {
	if len(fr.Payload) < minPayloadBytes {
		return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
			fmt.Sprintf("%s payload shorter than codec tag", kind))
	}
	var tag [4]byte
	copy(tag[:], fr.Payload[:4])
	switch kind {
	case KindVideo:
		if tag != VideoPayloadTag {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				fmt.Sprintf("video payload tag %q is not %q", tag, VideoPayloadTag))
		}
		if len(fr.Payload) < 12 {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				"video payload header truncated")
		}
		w := binary.BigEndian.Uint16(fr.Payload[4:])
		h := binary.BigEndian.Uint16(fr.Payload[6:])
		if fr.Width != 0 && w != fr.Width || fr.Height != 0 && h != fr.Height {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				fmt.Sprintf("video payload declares %dx%d but frame declares %dx%d", w, h, fr.Width, fr.Height))
		}
		if idx := binary.BigEndian.Uint32(fr.Payload[8:]); uint64(idx) != fr.Seq {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				fmt.Sprintf("video payload carries frame index %d", idx))
		}
	case KindAudio:
		if tag != AudioPayloadTag {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				fmt.Sprintf("audio payload tag %q is not %q", tag, AudioPayloadTag))
		}
		if len(fr.Payload) < 10 {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				"audio payload header truncated")
		}
		if idx := binary.BigEndian.Uint32(fr.Payload[4:]); uint64(idx) != fr.Seq {
			return Corruption("model.ValidatePayload", fr.TrackID, fr.Seq, fr.Offset,
				fmt.Sprintf("audio payload carries frame index %d", idx))
		}
	default:
		return New(KindInput, "model.ValidatePayload", fmt.Sprintf("unknown track kind %d", uint8(kind)))
	}
	return nil
}
