package adapter

import (
	"encoding/binary"
	"io"

	"avpipe/internal/media"
)

// parseAudioV1 reads aligned PCM block descriptors and their samples.
// Audio blocks whose sequence matches a corrupt frame remain present but are
// flagged by the pipeline through frame-gap propagation.
func parseAudioV1(r io.ReadSeeker, h v1Header, size int64, res *ParseResult) {
	if h.flags&FlagHasAudio == 0 || h.audioOffset == 0 {
		return
	}
	off := int64(h.audioOffset)
	var head [8]byte
	if err := readFull(r, off, head[:], size); err != nil {
		res.Errors = append(res.Errors, media.NewError("parser", media.KindTruncated, off,
			media.ActionTruncate, "audio header unreadable: %v", err))
		return
	}
	var magic [4]byte
	copy(magic[:], head[0:4])
	if magic != MagicAudio {
		res.Errors = append(res.Errors, media.NewError("parser", "bad_audio_magic", off,
			media.ActionFail, "audio section marker invalid"))
		return
	}
	count := u32(head[4:8])
	descOff := off + 8
	for i := uint32(0); i < count; i++ {
		var db [AudioHeaderSize]byte
		if err := readFull(r, descOff, db[:], size); err != nil {
			res.Errors = append(res.Errors, media.NewError("parser", media.KindTruncated, descOff,
				media.ActionTruncate, "audio descriptor %d truncated", i))
			return
		}
		relOffset := u64(db[24:32])
		// DataOffset is relative to the start of the audio section; resolve
		// it to an absolute file offset with overflow checks.
		absOff, overflow := media.AddU64(h.audioOffset, relOffset)
		if overflow {
			res.Errors = append(res.Errors, media.NewError("parser", media.KindOverflow, descOff,
				media.ActionTruncate, "audio offset overflow"))
			return
		}
		ah := AudioHeader{
			AudioBlock: media.AudioBlock{
				Sequence:   u32(db[0:4]),
				PTS90k:     u64(db[4:12]),
				SampleRate: u32(db[12:16]),
				Layout:     media.ChannelLayout(db[16]),
			},
			DataOffset: absOff,
		}
		n := u32(db[20:24])
		byteLen, overflow := media.MulU64(uint64(n), 2)
		if overflow || !media.CheckBounds(absOff, byteLen, uint64(size)-FooterSize) {
			res.Errors = append(res.Errors, media.NewSeqError("parser", media.KindOutOfBounds,
				ah.Sequence, media.ActionTruncate, "pcm bytes out of bounds"))
			return
		}
		pcm := make([]byte, byteLen)
		if err := readFull(r, int64(absOff), pcm, size); err != nil {
			res.Errors = append(res.Errors, media.NewSeqError("parser", media.KindTruncated,
				ah.Sequence, media.ActionTruncate, "pcm block truncated: %v", err))
			return
		}
		ah.Samples = bytesToPCM16(pcm)
		res.Audio = append(res.Audio, ah)
		descOff += AudioHeaderSize
	}
}

func bytesToPCM16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.BigEndian.Uint16(b[2*i:]))
	}
	return out
}
