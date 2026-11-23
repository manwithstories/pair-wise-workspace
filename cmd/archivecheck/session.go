package main

import (
	"context"
	"path/filepath"

	"archivecheck/internal/parse"
	"archivecheck/internal/pipe"
	"archivecheck/internal/repair"
	"archivecheck/internal/testkit"
)

// session is the CLI's view of one generated corpus plus its bounded pipeline
// input. Files stay open for the lifetime of the command.
type session struct {
	work     string
	manifest *pipe.Manifest
	loaded   *pipe.LoadedInput
	ord      *repair.Ordered
	tasks    []pipe.Task
	ctx      context.Context
	cancel   context.CancelFunc
}

func newSession(work string) (*session, error) {
	layout, err := testkit.WriteDemoInputs(work)
	if err != nil {
		return nil, err
	}
	if _, err := testkit.WriteFaultInputs(work); err != nil {
		return nil, err
	}
	return loadSession(work, filepath.Join(work, "in"), layout.ManifestPath)
}

func loadSession(work, root, manifestPath string) (*session, error) {
	ctx, cancel := signalContext()
	s := &session{work: work, ctx: ctx, cancel: cancel}
	m, err := pipe.LoadManifest(root, manifestPath)
	if err != nil {
		cancel()
		return nil, err
	}
	loaded, err := pipe.LoadInputs(m)
	if err != nil {
		cancel()
		return nil, err
	}
	ord, err := repair.Order(loaded.OrderItems())
	if err != nil {
		loaded.Close()
		cancel()
		return nil, err
	}
	s.manifest = m
	s.loaded = loaded
	s.ord = ord
	s.tasks = loaded.Tasks(ord)
	return s, nil
}

func (s *session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.loaded != nil {
		s.loaded.Close()
	}
}

func (s *session) primaryInit() *parse.Init { return s.loaded.Inits[0] }
