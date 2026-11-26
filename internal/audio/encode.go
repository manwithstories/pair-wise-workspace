package audio

import (
	"bytes"
	"os"

	"pairflow/internal/media"
)

// EncodeWAV renders PCM into a complete in-memory WAV byte stream with the
// same canonical header the Writer produces. Used when a stage must hand a
// fully validated artifact to the store in one piece.
func EncodeWAV(pcm []int16, f Format) ([]byte, *media.Error) {
	if err := f.validate(""); err != nil {
		return nil, err
	}
	if len(pcm)%f.Channels != 0 {
		return nil, media.Input("wav.partial_frame",
			"sample count %d is not a whole %d-channel frame", len(pcm), f.Channels)
	}
	var buf bytes.Buffer
	// Reuse the Writer's header logic by staging to a pipe-like temp file is
	// overkill; build the canonical 44-byte header directly.
	hdr := Header(f, len(pcm)*2)
	buf.Write(hdr)
	for _, s := range pcm {
		buf.WriteByte(byte(s))
		buf.WriteByte(byte(s >> 8))
	}
	return buf.Bytes(), nil
}

// ReadAllWAV opens path and drains it in bounded blocks, returning the PCM and
// the stream format. Memory holds only the returned result, never a raw copy.
func ReadAllWAV(path string) ([]int16, Format, *media.Error) {
	r, err := Open(path)
	if err != nil {
		return nil, Format{}, err
	}
	defer r.Close()
	var out []int16
	for {
		block, rerr := r.ReadFrames(4096)
		if rerr != nil {
			break
		}
		out = append(out, block...)
	}
	return out, r.Format, nil
}

// WriteWAVFile writes PCM to path via the streaming Writer and fsyncs it.
func WriteWAVFile(path string, pcm []int16, f Format) *media.Error {
	w, err := Create(path, f)
	if err != nil {
		return err
	}
	if err := w.Append(pcm); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	file, oerr := os.Open(path)
	if oerr != nil {
		return media.Wrap(oerr, media.ClassInternal, "wav.sync_open", "cannot reopen wav").WithPath(path)
	}
	defer file.Close()
	if serr := file.Sync(); serr != nil {
		return media.Wrap(serr, media.ClassInternal, "wav.sync", "cannot fsync wav").WithPath(path)
	}
	return nil
}
