package main

import (
	"testing"

	"mediapipe/internal/media"
)

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"corruption", media.NewError(media.CodeTruncatedMoov, "x", nil), 2},
		{"timestamp", media.TimestampError(0, 9, "rollback"), 2},
		{"loss", media.NewError(media.CodeUnrecoverableLoss, "x", nil), 3},
		{"digest", media.NewError(media.CodeChecksumMismatch, "x", nil), 3},
		{"io", media.NewError(media.CodeIO, "x", nil), 4},
		{"checkpoint", media.NewError(media.CodeCheckpoint, "x", nil), 4},
		{"coded", codedExit{code: 3}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Fatalf("exitCode=%d want %d", got, tc.want)
			}
		})
	}
}
