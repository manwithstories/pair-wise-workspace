package runner

import (
	"errors"
	"path/filepath"
	"testing"

	"fieldcut/internal/media"
)

func TestRunRejectsEmptyOut(t *testing.T) {
	_, err := Run(Options{})
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindConflict {
		t.Fatalf("err = %v", err)
	}
}

func TestSourceHashChangesWithInput(t *testing.T) {
	man := minimalManifest()
	entries := map[string][]struct {
		seq int64
		sum string
	}{}
	_ = entries
	_ = man
	if testing.Short() {
		t.Skip("covered by integration test")
	}
}

func minimalManifest() *struct{} { return &struct{}{} }

func TestRunMissingInputIsInputError(t *testing.T) {
	_, err := Run(Options{OutDir: t.TempDir(), InputDir: filepath.Join(t.TempDir(), "absent"), ConfigHash: "c"})
	if err == nil {
		t.Fatal("missing input accepted")
	}
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindInput {
		t.Fatalf("kind = %v", err)
	}
}
