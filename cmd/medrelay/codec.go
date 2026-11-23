package main

// Frame/segment codec: frames are length-prefixed binary records; segment
// files are minimal MP4 (ftyp + mdat) written and validated via go-mp4.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"

	"medrelay/internal/media"
	"medrelay/internal/timeline"
)

// MaxSegmentBytes bounds accepted segment file size.
const MaxSegmentBytes = 4 << 20

func encodeFrames(w io.Writer, frames []media.Frame) error {
	var hdr [29]byte
	for _, f := range frames {
		hdr[0] = f.Kind
		binary.BigEndian.PutUint64(hdr[1:9], uint64(f.PTS))
		binary.BigEndian.PutUint64(hdr[9:17], uint64(f.Dur))
		binary.BigEndian.PutUint64(hdr[17:25], uint64(f.TS))
		binary.BigEndian.PutUint32(hdr[25:29], uint32(len(f.Data)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(f.Data); err != nil {
			return err
		}
	}
	return nil
}

func decodeFrames(r io.Reader) ([]media.Frame, error) {
	var out []media.Frame
	var hdr [29]byte
	for {
		_, err := io.ReadFull(r, hdr[:])
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		f := media.Frame{
			Kind: hdr[0],
			PTS:  int64(binary.BigEndian.Uint64(hdr[1:9])),
			Dur:  int64(binary.BigEndian.Uint64(hdr[9:17])),
			TS:   timeline.Timescale(binary.BigEndian.Uint64(hdr[17:25])),
		}
		n := binary.BigEndian.Uint32(hdr[25:29])
		if n > MaxSegmentBytes {
			return nil, fmt.Errorf("frame payload %d exceeds limit", n)
		}
		f.Data = make([]byte, n)
		if _, err := io.ReadFull(r, f.Data); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
}

func writeFramesFile(path string, frames []media.Frame) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return encodeFrames(f, frames)
}

func readFramesFile(path string) ([]media.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return decodeFrames(f)
}

// writeSegmentFile writes frames as ftyp + mdat.
func writeSegmentFile(path string, frames []media.Frame) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := mp4.NewWriter(f)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType("ftyp")}); err != nil {
		return err
	}
	if _, err := w.Write([]byte("isom0000isom")); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType("mdat")}); err != nil {
		return err
	}
	if err := encodeFrames(w, frames); err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

// readSegmentFile validates the box structure and returns mdat frames.
// A truncated or oversized segment yields a located ErrTruncated error.
func readSegmentFile(path, uri string, startMs, endMs int64) ([]media.Frame, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, timeline.New(timeline.ErrMissingRef, uri, startMs, endMs, err.Error())
	}
	if st.Size() > MaxSegmentBytes {
		return nil, timeline.New(timeline.ErrLimit, uri, startMs, endMs,
			fmt.Sprintf("segment %d bytes exceeds limit %d", st.Size(), MaxSegmentBytes))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type box struct {
		off, size, hdr uint64
		typ            mp4.BoxType
	}
	var boxes []box
	_, walkErr := mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		boxes = append(boxes, box{bi.Offset, bi.Size, bi.HeaderSize, bi.Type})
		return nil, nil
	})
	if walkErr != nil {
		return nil, timeline.New(timeline.ErrTruncated, uri, startMs, endMs, walkErr.Error())
	}
	if len(boxes) < 2 {
		return nil, timeline.New(timeline.ErrTruncated, uri, startMs, endMs,
			fmt.Sprintf("only %d top-level boxes", len(boxes)))
	}
	for _, b := range boxes {
		if b.off+b.size > uint64(st.Size()) {
			return nil, timeline.New(timeline.ErrTruncated, uri, startMs, endMs,
				fmt.Sprintf("box %s at %d overruns file size %d", b.typ.String(), b.off, st.Size()))
		}
	}
	last := boxes[len(boxes)-1]
	if last.off+last.size != uint64(st.Size()) {
		return nil, timeline.New(timeline.ErrTruncated, uri, startMs, endMs,
			"trailing bytes after last box")
	}
	var mdat *box
	for i := range boxes {
		if boxes[i].typ == mp4.StrToBoxType("mdat") {
			mdat = &boxes[i]
		}
	}
	if mdat == nil {
		return nil, timeline.New(timeline.ErrMissingRef, uri, startMs, endMs, "no mdat box")
	}
	if _, err := f.Seek(int64(mdat.off+mdat.hdr), io.SeekStart); err != nil {
		return nil, err
	}
	payload := make([]byte, mdat.size-mdat.hdr)
	if _, err := io.ReadFull(f, payload); err != nil {
		return nil, timeline.New(timeline.ErrTruncated, uri, startMs, endMs, err.Error())
	}
	return decodeFrames(bytes.NewReader(payload))
}
