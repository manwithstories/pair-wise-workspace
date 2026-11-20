package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mediamend/mediamend/internal/fixture"
	"github.com/mediamend/mediamend/internal/index"
)

// loadProject reads the project manifest written by the fixture generator.
func loadProject(root string) (*fixture.Project, error) {
	data, err := os.ReadFile(filepath.Join(root, "project.json"))
	if err != nil {
		return nil, err
	}
	var proj fixture.Project
	if err := json.Unmarshal(data, &proj); err != nil {
		return nil, err
	}
	return &proj, nil
}

// loadIndex reconstructs an in-memory index handle for an existing MP4 so the
// verify subcommand can re-run seek checks without re-running the pipeline.
// The entry table is rebuilt by scanning the file's sample table; on any
// structural problem it returns nil and the caller reports a bounds fault.
func loadIndex(path, trackID string) *index.Index {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	idx := &index.Index{TrackID: trackID, File: path}
	entries, err := index.ReadEntries(f)
	if err != nil {
		return nil
	}
	idx.Entries = entries
	if len(entries) > 0 {
		idx.FirstSeq = entries[0].Seq
		idx.LastSeq = entries[len(entries)-1].Seq
	}
	return idx
}
