package pipeline_test

import (
	"errors"
	"path/filepath"
	"testing"

	"mediad/internal/model"
	"mediad/internal/pipeline"
)

func TestSafeJoinRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := pipeline.SafeJoin(root, "clean.mp4"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.mp4", "a/../../escape.mp4", "/absolute/path.mp4"} {
		_, err := pipeline.SafeJoin(root, name)
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodePathEscape {
			t.Fatalf("name %q: expected path_escape, got %v", name, err)
		}
	}
	got, err := pipeline.SafeJoin(root, filepath.Join("nested", "..", "clean.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "clean.mp4") {
		t.Fatalf("normalized path=%q", got)
	}
}
