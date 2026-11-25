package pipeline

import (
	"path/filepath"

	"mediqc/internal/audit"
	"mediqc/internal/checkpoint"
	"mediqc/internal/media"
)

func digestFromBytes(rel string, b []byte) audit.FileDigest {
	return audit.FileDigest{Path: rel, Bytes: len(b), SHA256: audit.HashBytes(b)}
}

func commitChunk(cm *audit.CaseManifest, store *checkpoint.Store, c checkpoint.Chunk) *media.Error {
	duplicate, err := store.Commit(c)
	if err != nil {
		return err
	}
	cm.Chunks = append(cm.Chunks, audit.ChunkDigest{
		ID: c.ID, Index: c.Index, Bytes: c.Bytes, SHA256: c.SHA256, Replayed: duplicate,
	})
	return nil
}

func refreshCheckpointDigest(outRoot string, cm *audit.CaseManifest) *media.Error {
	n, hash, err := audit.HashFile(filepath.Join(outRoot, filepath.FromSlash(cm.Checkpoint.Path)))
	if err != nil {
		return &media.Error{Code: media.CodeCheckpoint, Component: "pipeline", Message: err.Error()}
	}
	cm.Checkpoint.Bytes = n
	cm.Checkpoint.SHA256 = hash
	return nil
}
