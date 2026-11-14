// Package store persists reassembled frames and an append-only index that
// makes capture resumption restart-proof: each frame's sequence range and
// digest is recorded before the frame bytes are reused.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	rtppkg "github.com/example/rtpcap/internal/rtp"
)

// IndexEntry is one line of index.jsonl.
type IndexEntry struct {
	FrameIndex  int    `json:"frame_index"`
	FirstSeq    uint16 `json:"first_seq"`
	LastSeq     uint16 `json:"last_seq"`
	Timestamp   uint32 `json:"timestamp"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
	PacketCount int    `json:"packet_count"`
	File        string `json:"file"`
}

// FrameStore writes frame payload files and a JSONL index. It is safe to
// reopen an existing directory: the index is replayed and appending resumes
// at the next frame index without duplicating any frame.
type FrameStore struct {
	dir       string
	indexPath string
	entries   []IndexEntry
	indexFile *os.File
	writer    *bufio.Writer
}

// Open creates the output directory and replays any existing index.
func Open(dir string) (*FrameStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "frames"), 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	fs := &FrameStore{
		dir:       dir,
		indexPath: filepath.Join(dir, "index.jsonl"),
	}
	if err := fs.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(fs.indexPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fs.indexFile = f
	fs.writer = bufio.NewWriter(f)
	return fs, nil
}

func (fs *FrameStore) replay() error {
	f, err := os.Open(fs.indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var entry IndexEntry
		if err := dec.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("corrupt index: %w", err)
		}
		fs.entries = append(fs.entries, entry)
	}
	return nil
}

// NextFrameIndex returns the frame index the next WriteFrame will use.
func (fs *FrameStore) NextFrameIndex() int {
	return len(fs.entries)
}

// LastContiguousSeq returns the last sequence covered by stored frames, or
// -1 when nothing has been stored yet.
func (fs *FrameStore) LastContiguousSeq() int {
	if len(fs.entries) == 0 {
		return -1
	}
	return int(fs.entries[len(fs.entries)-1].LastSeq)
}

// WriteFrame persists one frame's payload and appends its index entry.
func (fs *FrameStore) WriteFrame(fr rtppkg.Frame) error {
	name := fmt.Sprintf("frames/frame-%06d.bin", fr.Index)
	path := filepath.Join(fs.dir, name)
	if err := os.WriteFile(path, fr.Payload, 0o644); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	entry := IndexEntry{
		FrameIndex:  fr.Index,
		FirstSeq:    fr.FirstSeq,
		LastSeq:     fr.LastSeq,
		Timestamp:   fr.Timestamp,
		Bytes:       fr.Bytes,
		SHA256:      fr.SHA256,
		PacketCount: fr.PacketCount,
		File:        name,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := fs.writer.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := fs.writer.Flush(); err != nil {
		return err
	}
	fs.entries = append(fs.entries, entry)
	return nil
}

// Close flushes and closes the index file.
func (fs *FrameStore) Close() error {
	if fs.writer != nil {
		if err := fs.writer.Flush(); err != nil {
			return err
		}
	}
	if fs.indexFile != nil {
		return fs.indexFile.Close()
	}
	return nil
}

// Entries returns a copy of all replayed/appended index entries.
func (fs *FrameStore) Entries() []IndexEntry {
	out := make([]IndexEntry, len(fs.entries))
	copy(out, fs.entries)
	return out
}
