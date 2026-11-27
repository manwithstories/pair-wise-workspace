// Package parse implements chunked, offset-preserving parsers for
// WebVTT captions and timed metadata (chapters, ID3v2, emsg, SEI).
package parse

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

// Emitter receives parsed events; a non-nil return aborts parsing.
type Emitter func(model.Event) error

// lineReader reads lines while tracking absolute byte offsets.
type lineReader struct {
	br  *bufio.Reader
	off int64
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, 8<<10)}
}

// next returns the next line without its newline, and its start offset.
func (l *lineReader) next() (string, int64, error) {
	start := l.off
	line, err := l.br.ReadString('\n')
	l.off += int64(len(line))
	if err == io.EOF && line == "" {
		return "", start, io.EOF
	}
	if err != nil && err != io.EOF {
		return "", start, err
	}
	return strings.TrimRight(line, "\r\n"), start, nil
}

// ParseVTT incrementally parses a WebVTT stream, emitting one event
// per cue and preserving each cue's source byte offset.
func ParseVTT(ctx context.Context, r io.Reader, track int, emit Emitter) error {
	lr := newLineReader(r)
	header, _, err := lr.next()
	if err != nil {
		return &model.TrackError{Track: track, Stage: model.StageParse, Offset: 0,
			Msg: "missing WEBVTT header"}
	}
	if !strings.HasPrefix(strings.TrimPrefix(header, "\ufeff"), "WEBVTT") {
		return &model.TrackError{Track: track, Stage: model.StageParse, Offset: 0,
			Msg: "missing WEBVTT header"}
	}
	var seq int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var block []string
		var blockStart int64
		blockBytes := 0
		for {
			line, off, err := lr.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if line == "" {
				if len(block) > 0 {
					break
				}
				continue
			}
			if len(block) == 0 {
				blockStart = off
			}
			blockBytes += len(line) + 1
			if blockBytes > model.MaxRecordBytes {
				return &model.TrackError{Track: track, Stage: model.StageParse,
					Offset: blockStart, Msg: "cue exceeds 64 KiB record limit"}
			}
			block = append(block, line)
		}
		if len(block) == 0 {
			return nil
		}
		ev, err := parseCue(block, blockStart, track, seq)
		if err != nil {
			return err
		}
		if err := emit(ev); err != nil {
			return err
		}
		seq++
	}
}

func parseCue(block []string, start int64, track int, seq int64) (model.Event, error) {
	fail := func(msg string) (model.Event, error) {
		return model.Event{}, &model.TrackError{Track: track, Stage: model.StageParse,
			Offset: start, Msg: msg}
	}
	idx := 0
	if !strings.Contains(block[0], "-->") {
		idx = 1 // optional cue identifier line
	}
	if idx >= len(block) || !strings.Contains(block[idx], "-->") {
		return fail("incomplete cue: missing timing line")
	}
	parts := strings.SplitN(block[idx], "-->", 2)
	t0, err := parseVTTTime(strings.TrimSpace(parts[0]))
	if err != nil {
		return fail("bad cue start: " + err.Error())
	}
	fields := strings.Fields(strings.TrimSpace(parts[1]))
	if len(fields) == 0 {
		return fail("bad cue end time")
	}
	t1, err := parseVTTTime(fields[0])
	if err != nil {
		return fail("bad cue end: " + err.Error())
	}
	dur, err := t1.Sub(t0)
	if err != nil {
		return fail(err.Error())
	}
	if err := clock.RequireNonNegative(dur); err != nil {
		return fail(err.Error())
	}
	payload := strings.Join(block[idx+1:], "\n")
	if payload == "" {
		return fail("incomplete cue: missing payload")
	}
	return model.Event{Track: track, Seq: seq, Start: t0, Dur: dur,
		Kind: "caption", Payload: payload, Offset: start}, nil
}

// parseVTTTime parses "HH:MM:SS.mmm" or "MM:SS.mmm" into milliseconds.
func parseVTTTime(s string) (clock.Time, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return clock.Time{}, fmt.Errorf("invalid timestamp %q", s)
	}
	secPart := parts[len(parts)-1]
	var h, m int64
	if len(parts) == 3 {
		h, _ = strconv.ParseInt(parts[0], 10, 64)
		m, _ = strconv.ParseInt(parts[1], 10, 64)
	} else {
		m, _ = strconv.ParseInt(parts[0], 10, 64)
	}
	si := strings.SplitN(secPart, ".", 2)
	if len(si) != 2 || len(si[1]) != 3 {
		return clock.Time{}, fmt.Errorf("invalid timestamp %q", s)
	}
	sec, err1 := strconv.ParseInt(si[0], 10, 64)
	ms, err2 := strconv.ParseInt(si[1], 10, 64)
	if err1 != nil || err2 != nil || h < 0 || m < 0 || sec < 0 {
		return clock.Time{}, fmt.Errorf("invalid timestamp %q", s)
	}
	return clock.New((h*60+m)*60*1000+sec*1000+ms, 1000)
}
