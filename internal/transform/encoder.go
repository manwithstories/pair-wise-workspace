package transform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"relayaudit/internal/fault"
	"relayaudit/internal/wire"
)

// Encoder incrementally assembles the repaired record stream. It is also able
// to resume from a previously committed prefix: the prefix is re-parsed and
// the index entries are rebuilt from the bytes that actually survived.
//
// A running SHA-256 over the emitted stream is maintained as records are
// appended, so committing a checkpoint costs one digest per chunk rather than
// one re-hash of the whole prefix.
type Encoder struct {
	stream  []byte
	entries []wire.Entry
	kept    []*wire.Record
	dropped []int64
	hash    hash.Hash
}

// NewEncoder returns an empty encoder.
func NewEncoder() *Encoder { return &Encoder{hash: sha256.New()} }

// Digest returns the hex SHA-256 of the stream emitted so far.
func (e *Encoder) Digest() string { return hex.EncodeToString(e.hash.Sum(nil)) }

// Restore seeds the encoder from a validated staged prefix.
func (e *Encoder) Restore(prefix []byte) error {
	if e.hash == nil {
		e.hash = sha256.New()
	}
	e.stream = append([]byte(nil), prefix...)
	e.hash.Write(prefix)
	rd := wire.NewRecordReader(e.stream, 0, int64(len(e.stream)))
	for {
		rec, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			off := int64(0)
			if f, ok := fault.As(err); ok {
				off = f.Offset()
			}
			return fault.Wrap(fault.Checkpoint, off, err, "staged prefix does not decode: %v", err)
		}
		e.entries = append(e.entries, wire.Entry{
			Offset: rec.Offset, Length: uint32(rec.Size()),
			Track: rec.Track, SeqLow: uint16(rec.Seq),
		})
		e.kept = append(e.kept, rec)
	}
	return nil
}

// Add appends one record to the stream.
func (e *Encoder) Add(rec *wire.Record) {
	if e.hash == nil {
		e.hash = sha256.New()
	}
	before := len(e.stream)
	off := int64(before)
	e.stream = rec.Marshal(e.stream)
	e.hash.Write(e.stream[before:])
	outRec := *rec
	outRec.Offset = off
	e.entries = append(e.entries, wire.Entry{
		Offset: off, Length: uint32(outRec.Size()),
		Track: outRec.Track, SeqLow: uint16(outRec.Seq),
	})
	e.kept = append(e.kept, &outRec)
}

// Drop records an excluded source offset.
func (e *Encoder) Drop(offset int64) { e.dropped = append(e.dropped, offset) }

// Kept returns the number of emitted records.
func (e *Encoder) Kept() int { return len(e.kept) }

// Dropped returns the number of isolated records.
func (e *Encoder) Dropped() int { return len(e.dropped) }

// Bytes returns the staged record stream (without header or index).
func (e *Encoder) Bytes() []byte { return e.stream }

// Records returns the emitted records in output order.
func (e *Encoder) Records() []*wire.Record { return e.kept }

// Finalize wraps the stream in a fresh RLY1 container with a consistent index.
// Index entry offsets are rewritten to final file coordinates so the container
// verifies standalone.
func (e *Encoder) Finalize(trackCount uint8) ([]byte, error) {
	if trackCount == 0 {
		trackCount = wire.DefaultTracks
	}
	pages := wire.PagesFor(len(e.entries))
	dataStart := int64(wire.HeaderSize) + int64(pages)*wire.PageSize
	out := make([]byte, dataStart+int64(len(e.stream)))
	hdr := wire.EncodeHeader(wire.Header{
		Version: wire.Version, Tracks: trackCount,
		RecordCount: uint32(len(e.entries)), PageCount: uint32(pages), DataStart: dataStart,
	})
	entries := make([]wire.Entry, len(e.entries))
	copy(entries, e.entries)
	for i := range entries {
		entries[i].Offset += dataStart
	}
	copy(out[:wire.HeaderSize], hdr[:])
	copy(out[wire.HeaderSize:dataStart], wire.EncodeIndex(entries))
	copy(out[dataStart:], e.stream)
	// Shift the in-memory record view to match the final coordinates.
	for _, rec := range e.kept {
		rec.Offset += dataStart
	}
	for i := range e.entries {
		e.entries[i] = entries[i]
	}
	if _, err := wire.Scan(out); err != nil {
		return nil, fmt.Errorf("transform: finalised output does not scan: %w", err)
	}
	return out, nil
}
