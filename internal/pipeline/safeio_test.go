package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/mediad/internal/model"
)

func TestRunRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "escape")
	target := t.TempDir()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{Root: root, WorkDir: "escape", Seed: 1,
		VideoSamples: 4})
	if err == nil {
		t.Fatal("symlinked work dir must be rejected")
	}
	me, ok := model.AsError(err)
	if !ok || me.Kind != model.KindArgument {
		t.Fatalf("want argument error, got %v", err)
	}
}

func TestRunRejectsParentTraversal(t *testing.T) {
	root := t.TempDir()
	_, err := Run(Options{Root: root, WorkDir: "../escape-work",
		Seed: 1, VideoSamples: 4})
	if err == nil {
		t.Fatal("parent-traversing work dir must be rejected")
	}
	if me, ok := model.AsError(err); !ok || me.Kind != model.KindArgument {
		t.Fatalf("want argument error, got %v", err)
	}
}

func TestRunAcceptsInRootWorkDir(t *testing.T) {
	root := t.TempDir()
	if _, err := Run(Options{Root: root, WorkDir: "run1", Seed: 1,
		VideoSamples: 4}); err != nil {
		t.Fatalf("in-root work dir must be accepted: %v", err)
	}
}
