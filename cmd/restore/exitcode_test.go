package main

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/archive/tsrestore/internal/cache"
	"github.com/archive/tsrestore/internal/checkpoint"
	"github.com/archive/tsrestore/internal/pipeline"
	"github.com/archive/tsrestore/internal/sample"
)

// garbageStream returns length-prefixed datagrams that are neither valid RTP
// nor RTCP (version field zero), i.e. unrecoverable input.
func garbageStream(n int) []byte {
	var out []byte
	for i := 0; i < n; i++ {
		body := []byte{0x00, 0x21, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0xAA, 0xBB}
		var prefix [2]byte
		binary.BigEndian.PutUint16(prefix[:], uint16(len(body)))
		out = append(out, prefix[:]...)
		out = append(out, body...)
	}
	return out
}

// An unrecoverable input yields no segments, so the acceptance contract cannot
// be met and runVerify's assertion path returns exit code 2.
func TestUnrecoverableInputFailsAcceptanceWithExit2(t *testing.T) {
	dir := t.TempDir()
	cas, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	ckpt, err := checkpoint.Open(filepath.Join(dir, "ckpt.json"), []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	ins := []*pipeline.RenditionInput{{
		Name: "v1", Datagrams: garbageStream(20),
	}}
	res, err := pipeline.Run(context.Background(), ins, ckpt, cas,
		pipeline.Config{OutDir: dir, Seed: 1, ProgramSecs: 12})
	if err != nil {
		// A hard pipeline error is itself mapped through classify() to exit 2.
		if got := classify(err); got != exitFatal {
			t.Fatalf("unrecoverable pipeline error classified %d, want %d", got, exitFatal)
		}
		return
	}
	// Corrupt datagrams are skipped, leaving zero restored segments.
	m := computeMetrics(res, 12)
	normal := &normalSummary{
		expectedPerRendition: 12 / sample.SegmentSeconds,
		metrics:              m,
		hashes:               collectHashes(res),
	}
	if code := assertAll(&verifyFlags{seconds: 12}, normal, &m, nil); code != exitFatal {
		t.Fatalf("zero-segment run must fail acceptance with exit %d, got %d", exitFatal, code)
	}
}
