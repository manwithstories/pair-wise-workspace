package parse

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

// parseTextPayload parses "start=<n>/<d>;dur=<n>/<d>;label=<text>",
// the timed payload convention shared by ID3 TXXX frames and SEI
// user data in this toolchain.
func parseTextPayload(s string) (start, dur clock.Time, label string, err error) {
	parts := strings.SplitN(s, ";", 3)
	if len(parts) != 3 ||
		!strings.HasPrefix(parts[0], "start=") ||
		!strings.HasPrefix(parts[1], "dur=") ||
		!strings.HasPrefix(parts[2], "label=") {
		return clock.Time{}, clock.Time{}, "", fmt.Errorf("malformed timed payload")
	}
	if start, err = parseRat(strings.TrimPrefix(parts[0], "start=")); err != nil {
		return clock.Time{}, clock.Time{}, "", err
	}
	if dur, err = parseRat(strings.TrimPrefix(parts[1], "dur=")); err != nil {
		return clock.Time{}, clock.Time{}, "", err
	}
	if err = clock.RequireNonNegative(dur); err != nil {
		return clock.Time{}, clock.Time{}, "", err
	}
	return start, dur, strings.TrimPrefix(parts[2], "label="), nil
}

func parseRat(s string) (clock.Time, error) {
	p := strings.Split(s, "/")
	if len(p) != 2 {
		return clock.Time{}, fmt.Errorf("invalid rational %q", s)
	}
	n, err1 := strconv.ParseInt(p[0], 10, 64)
	d, err2 := strconv.ParseInt(p[1], 10, 64)
	if err1 != nil || err2 != nil {
		return clock.Time{}, fmt.Errorf("invalid rational %q", s)
	}
	return clock.New(n, d)
}

// chapter is one line of the chapters JSONL track.
type chapter struct {
	ID      int64  `json:"id"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Title   string `json:"title"`
}

// ParseChapters incrementally parses chapter JSONL, one event per line.
func ParseChapters(ctx context.Context, r io.Reader, track int, emit Emitter) error {
	lr := newLineReader(r)
	var seq int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, off, err := lr.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > model.MaxRecordBytes {
			return &model.TrackError{Track: track, Stage: model.StageParse,
				Offset: off, Msg: "chapter line exceeds 64 KiB record limit"}
		}
		var c chapter
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse,
				Offset: off, Msg: "bad chapter JSON: " + err.Error()}
		}
		start, err := clock.New(c.StartMS, 1000)
		if err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off, Msg: err.Error()}
		}
		end, err := clock.New(c.EndMS, 1000)
		if err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off, Msg: err.Error()}
		}
		dur, err := end.Sub(start)
		if err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off, Msg: err.Error()}
		}
		if err := clock.RequireNonNegative(dur); err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off, Msg: err.Error()}
		}
		if err := emit(model.Event{Track: track, Seq: seq, Start: start, Dur: dur,
			Kind: "chapter", Payload: c.Title, Offset: off}); err != nil {
			return err
		}
		seq++
	}
}

func syncsafe(b []byte) int64 {
	return int64(b[0]&0x7f)<<21 | int64(b[1]&0x7f)<<14 |
		int64(b[2]&0x7f)<<7 | int64(b[3]&0x7f)
}

// ParseID3 incrementally parses an ID3v2 tag, emitting one event per
// TXXX frame carrying a timed text payload.
func ParseID3(ctx context.Context, r io.Reader, track int, emit Emitter) error {
	br := bufio.NewReaderSize(r, 8<<10)
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return &model.TrackError{Track: track, Stage: model.StageParse, Offset: 0,
			Msg: "truncated ID3 header"}
	}
	if string(hdr[:3]) != "ID3" {
		return &model.TrackError{Track: track, Stage: model.StageParse, Offset: 0,
			Msg: "missing ID3v2 magic"}
	}
	ver := hdr[3]
	tagEnd := int64(10) + syncsafe(hdr[6:10])
	off := int64(10)
	var seq int64
	for off < tagEnd {
		if err := ctx.Err(); err != nil {
			return err
		}
		fh := make([]byte, 10)
		if _, err := io.ReadFull(br, fh); err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off,
				Msg: "truncated ID3 frame header"}
		}
		id := string(fh[:4])
		if id == "\x00\x00\x00\x00" {
			return nil // padding
		}
		var size int64
		if ver >= 4 {
			size = syncsafe(fh[4:8])
		} else {
			size = int64(binary.BigEndian.Uint32(fh[4:8]))
		}
		if size <= 0 || size > model.MaxRecordBytes || off+10+size > tagEnd {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off,
				Msg: fmt.Sprintf("invalid ID3 frame size %d", size)}
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off,
				Msg: "truncated ID3 frame payload"}
		}
		if id == "TXXX" {
			start, dur, label, err := parseTextPayload(string(payload))
			if err != nil {
				return &model.TrackError{Track: track, Stage: model.StageParse,
					Offset: off, Msg: err.Error()}
			}
			if err := emit(model.Event{Track: track, Seq: seq, Start: start, Dur: dur,
				Kind: "id3", Payload: label, Offset: off}); err != nil {
				return err
			}
			seq++
		}
		off += 10 + size
	}
	return nil
}
