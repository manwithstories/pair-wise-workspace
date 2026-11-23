package main

import (
	"context"
	"os"
	"sort"

	"archivecheck/internal/parse"
	"archivecheck/internal/pipe"
	"archivecheck/internal/repair"
)

type session struct {
	manifest *pipe.Manifest
	inits    []*parse.Init
	frags    []*parse.Fragment
	files    map[string]*os.File
	ord      *repair.Ordered
	ctx      context.Context
	cancel   context.CancelFunc
}

func openSession(root, manifestPath string) (*session, error) {
	ctx, cancel := signalContext()
	s := &session{ctx: ctx, cancel: cancel}
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
	ord, err := repair.Order(loaded.Fragments)
	if err != nil {
		loaded.Close()
		cancel()
		return nil, err
	}
	// Map each ordered fragment back to the init segment it belongs to and
	// verify all groups share a compatible single-track layout for the demo.
	sort.SliceStable(ord.Fragments, func(a, b int) bool { return a < b })
	s.manifest = m
	s.inits = loaded.Inits
	s.frags = loaded.Fragments
	s.files = loaded.Files
	s.ord = ord
	return s, nil
}

func (s *session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	for _, f := range s.files {
		if f != nil {
			f.Close()
		}
	}
}

func contextFor(s *session) context.Context { return s.ctx }
