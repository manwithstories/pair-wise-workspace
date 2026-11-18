package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// RawRecord 是 JSONL 单行的解码形态；SyncID 用指针以区分“缺省”与空串。
type RawRecord struct {
	Track string  `json:"track"`
	Epoch int64   `json:"epoch"`
	Seq   int64   `json:"seq"`
	PTS   int64   `json:"pts"`
	Num   int64   `json:"num"`
	Den   int64   `json:"den"`
	Bits  int     `json:"bits"`
	Sync  *string `json:"sync_id"`
}

// Record 是字段校验后的记录，Line 为 1 基输入行号，错误一律携带行号。
type Record struct {
	Line    int
	Track   string
	Epoch   int64
	Seq     int64
	PTS     int64
	TB      Timebase
	Bits    int
	SyncID  string
	HasSync bool
}

// LineError 是带行号的输入错误。
type LineError struct {
	Line int
	Msg  string
}

func (e *LineError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
}

func lineError(line int, format string, args ...any) error {
	return &LineError{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// openInput 打开输入文件；“-”表示标准输入。
func openInput(path string) (*os.File, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

// ScanRecords 流式逐行解析 JSONL：跳过空行，拒绝未知字段与尾随内容，
// 并完成全部字段级校验。回调在每条记录上调用，避免一次性载入内存。
func ScanRecords(r io.Reader, fn func(*Record) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var rr RawRecord
		if err := dec.Decode(&rr); err != nil {
			return lineError(line, "invalid JSON: %v", err)
		}
		// 一行只允许一个 JSON 值，尾随 token 同样视为输入错误。
		var extra json.RawMessage
		if err := dec.Decode(&extra); err != io.EOF {
			return lineError(line, "trailing data after JSON object")
		}
		rec, err := validate(rr, line)
		if err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	return nil
}

func validate(rr RawRecord, line int) (*Record, error) {
	if rr.Track != "audio" && rr.Track != "video" {
		return nil, lineError(line, "track must be audio or video, got %q", rr.Track)
	}
	if rr.Bits != 32 && rr.Bits != 33 {
		return nil, lineError(line, "bits must be 32 or 33, got %d", rr.Bits)
	}
	tb := Timebase{Num: rr.Num, Den: rr.Den}
	if !tb.Valid() {
		return nil, lineError(line, "illegal timebase num/den: %d/%d", rr.Num, rr.Den)
	}
	if rr.Epoch < 0 {
		return nil, lineError(line, "epoch must be non-negative, got %d", rr.Epoch)
	}
	if rr.Seq < 0 {
		return nil, lineError(line, "seq must be non-negative, got %d", rr.Seq)
	}
	if rr.PTS < 0 {
		return nil, lineError(line, "pts must be non-negative counter value, got %d", rr.PTS)
	}
	rec := &Record{
		Line: line, Track: rr.Track, Epoch: rr.Epoch, Seq: rr.Seq,
		PTS: rr.PTS, TB: tb, Bits: rr.Bits,
	}
	if rr.Sync != nil {
		rec.HasSync = true
		rec.SyncID = *rr.Sync
		if rec.SyncID == "" {
			return nil, lineError(line, "sync_id present but empty")
		}
	}
	return rec, nil
}
