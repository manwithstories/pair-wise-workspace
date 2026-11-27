package parse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// ParseEMSG incrementally parses MP4 emsg boxes (version 1). Unknown
// box types are skipped; a truncated box is reported at its offset.
func ParseEMSG(ctx context.Context, r io.Reader, track int, emit Emitter) error {
	br := bufio.NewReaderSize(r, 8<<10)
	off := int64(0)
	var seq int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr := make([]byte, 8)
		n, err := io.ReadFull(br, hdr)
		if err == io.EOF && n == 0 {
			return nil
		}
		if err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off,
				Msg: "truncated box header"}
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		if size < 8 || size > model.MaxRecordBytes {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off,
				Msg: fmt.Sprintf("invalid box size %d", size)}
		}
		body := make([]byte, size-8)
		if _, err := io.ReadFull(br, body); err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse, Offset: off,
				Msg: "truncated " + typ + " box"}
		}
		if typ == "emsg" {
			ev, err := parseEMSGBody(body, track, off, seq)
			if err != nil {
				return err
			}
			if err := emit(ev); err != nil {
				return err
			}
			seq++
		}
		off += size
	}
}

func parseEMSGBody(body []byte, track int, off int64, seq int64) (model.Event, error) {
	fail := func(msg string) (model.Event, error) {
		return model.Event{}, &model.TrackError{Track: track, Stage: model.StageParse,
			Offset: off, Msg: msg}
	}
	if len(body) < 24 {
		return fail("truncated emsg body")
	}
	if body[0] != 1 {
		return fail(fmt.Sprintf("unsupported emsg version %d", body[0]))
	}
	timescale := int64(binary.BigEndian.Uint32(body[4:8]))
	pts := binary.BigEndian.Uint64(body[8:16])
	if pts > math.MaxInt64 {
		return fail("emsg presentation time overflows int64")
	}
	dur := int64(binary.BigEndian.Uint32(body[16:20]))
	start, err := clock.New(int64(pts), timescale)
	if err != nil {
		return fail(err.Error())
	}
	d, err := clock.New(dur, timescale)
	if err != nil {
		return fail(err.Error())
	}
	rest := body[24:]
	ni := bytes.IndexByte(rest, 0)
	if ni < 0 {
		return fail("emsg missing scheme terminator")
	}
	rest = rest[ni+1:]
	vi := bytes.IndexByte(rest, 0)
	if vi < 0 {
		return fail("emsg missing value terminator")
	}
	value := string(rest[:vi])
	msgData := string(rest[vi+1:])
	return model.Event{Track: track, Seq: seq, Start: start, Dur: d,
		Kind: "emsg", Payload: value + "|" + msgData, Offset: off}, nil
}

// nextNAL scans an Annex-B byte stream for the next start code and
// returns the NAL unit up to (but excluding) the following start code.
func nextNAL(br *bufio.Reader, off *int64) (nal []byte, start int64, err error) {
	var w [3]byte
	found := false
	for !found {
		b, err := br.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		*off++
		w[0], w[1], w[2] = w[1], w[2], b
		found = w[0] == 0 && w[1] == 0 && w[2] == 1
	}
	start = *off - 3
	buf := make([]byte, 0, 256)
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			return buf, start, nil
		}
		if err != nil {
			return nil, 0, err
		}
		*off++
		if b == 0 {
			if p, perr := br.Peek(2); perr == nil && p[0] == 0 && p[1] == 1 {
				if uerr := br.UnreadByte(); uerr == nil {
					*off--
				}
				return buf, start, nil
			}
		}
		buf = append(buf, b)
		if len(buf) > model.MaxRecordBytes {
			return nil, start, fmt.Errorf("NAL unit exceeds 64 KiB record limit")
		}
	}
}

// ParseSEI incrementally parses Annex-B H.264 SEI NAL units, emitting
// one event per user_data_unregistered payload with a timed text body.
func ParseSEI(ctx context.Context, r io.Reader, track int, emit Emitter) error {
	br := bufio.NewReaderSize(r, 8<<10)
	off := int64(0)
	var seq int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nal, start, err := nextNAL(br, &off)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return &model.TrackError{Track: track, Stage: model.StageParse,
				Offset: start, Msg: err.Error()}
		}
		if len(nal) == 0 || nal[0]&0x1f != 6 {
			continue // not an SEI NAL
		}
		i := 1
		for i < len(nal) {
			ptype := 0
			for i < len(nal) && nal[i] == 0xff {
				ptype += 255
				i++
			}
			if i >= len(nal) {
				return &model.TrackError{Track: track, Stage: model.StageParse,
					Offset: start, Msg: "corrupt SEI payload type"}
			}
			ptype += int(nal[i])
			i++
			psize := 0
			for i < len(nal) && nal[i] == 0xff {
				psize += 255
				i++
			}
			if i >= len(nal) {
				return &model.TrackError{Track: track, Stage: model.StageParse,
					Offset: start, Msg: "corrupt SEI payload size"}
			}
			psize += int(nal[i])
			i++
			if psize < 0 || i+psize > len(nal) {
				return &model.TrackError{Track: track, Stage: model.StageParse,
					Offset: start, Msg: "corrupt SEI: payload overruns NAL unit"}
			}
			payload := nal[i : i+psize]
			i += psize
			if ptype != 5 || len(payload) < 16 {
				continue
			}
			st, dur, label, err := parseTextPayload(string(payload[16:]))
			if err != nil {
				return &model.TrackError{Track: track, Stage: model.StageParse,
					Offset: start, Msg: err.Error()}
			}
			if err := emit(model.Event{Track: track, Seq: seq, Start: st, Dur: dur,
				Kind: "sei", Payload: label, Offset: start}); err != nil {
				return err
			}
			seq++
		}
	}
}
