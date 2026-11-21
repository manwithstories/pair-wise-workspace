package txn

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

func (w *WAL) fileSize() int64 {
	st, err := w.f.Stat()
	if err != nil {
		return w.size
	}
	return st.Size()
}

func (w *WAL) readRecord(off int64) (*Record, int64, string) {
	if _, err := w.f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err.Error()
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(w.f, lenBuf[:]); err != nil {
		if err == io.EOF {
			return nil, off, ""
		}
		return nil, off, "torn frame length: " + err.Error()
	}
	bodyLen := binary.BigEndian.Uint32(lenBuf[:])
	if bodyLen == 0 || bodyLen > maxRecord {
		return nil, off, fmt.Sprintf("frame at %d declares implausible body length %d", off, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(w.f, body); err != nil {
		return nil, off, "torn frame body: " + err.Error()
	}
	var crcBuf [4]byte
	if _, err := io.ReadFull(w.f, crcBuf[:]); err != nil {
		return nil, off, "torn frame checksum: " + err.Error()
	}
	want := binary.BigEndian.Uint32(crcBuf[:])
	if got := crc32.ChecksumIEEE(body); got != want {
		return nil, off, fmt.Sprintf("frame at %d checksum %08x, want %08x", off, got, want)
	}
	if len(body) < 11 {
		return nil, off, fmt.Sprintf("frame at %d is too short", off)
	}
	kind := Kind(body[0])
	if kind < KindBegin || kind > KindRollback {
		return nil, off, fmt.Sprintf("frame at %d has unknown kind %d", off, kind)
	}
	stageLen := int(binary.BigEndian.Uint16(body[1:3]))
	if 3+stageLen+8 > len(body) {
		return nil, off, fmt.Sprintf("frame at %d stage length overruns body", off)
	}
	stage := string(body[3 : 3+stageLen])
	seq := binary.BigEndian.Uint64(body[3+stageLen : 11+stageLen])
	payload := append([]byte(nil), body[11+stageLen:]...)
	return &Record{
		Seq: seq, Kind: kind, Stage: stage, Payload: payload, Offset: off,
	}, off + int64(frameHead+int(bodyLen)+frameTail), ""
}
