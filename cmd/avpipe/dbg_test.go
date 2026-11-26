// Exit-code mapping tests for the avpipe CLI. (The file retains its original
// debug filename because the sandbox cannot rename it; the content is a
// permanent unit test, not throwaway diagnostics.)
package main

import (
	"errors"
	"testing"

	"avpipe/internal/media"
)

func TestMapExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitRuntime},
		{"config", media.NewError(media.ClassConfig, "test", "bad", 0, false), exitArgs},
		{"validation", media.NewError(media.ClassValidation, "test", "bad", 0, false), exitAsset},
		{"corrupt", media.NewError(media.ClassCorrupt, "test", "bad", 0, true), exitAsset},
		{"duplicate", media.NewError(media.ClassDuplicate, "test", "bad", 0, true), exitAsset},
		{"loss", media.NewError(media.ClassLoss, "test", "bad", 0, true), exitAsset},
		{"clock", media.NewError(media.ClassClock, "test", "bad", 0, true), exitAsset},
		{"encode", media.NewError(media.ClassEncode, "test", "bad", 0, false), exitAsset},
		{"index", media.NewError(media.ClassIndex, "test", "bad", 0, false), exitIndex},
		{"plain", errors.New("ordinary"), exitRuntime},
	}
	for _, tc := range cases {
		if got := mapExit(tc.err); got != tc.want {
			t.Errorf("%s: mapExit = %d, want %d", tc.name, got, tc.want)
		}
	}
}
