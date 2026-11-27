package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/demux"
	"relay/internal/fixture"
	"relay/internal/media"
	"relay/internal/remux"
)

func mustOpenFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func decodeManifest(t *testing.T, raw []byte) *remux.Manifest {
	t.Helper()
	var m remux.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// runOneShot 不带中断地跑一遍相同输入，作为恢复结果的对照基准。
func runOneShot(t *testing.T, fx *fixture.Outputs, out string) []*media.Frame {
	t.Helper()
	dir := filepath.Dir(out)
	e, err := New(context.Background(), Options{
		InputPath: fx.Live, OutputPath: out, MapPath: fx.Map,
		CheckpointPath: dir + "/oneshot.ckpt",
		ManifestPath:   dir + "/oneshot.json",
		MaxBuffer:      64 << 20,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	frames, err := demux.ReadAll(mustOpenFile(t, out), out)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}
