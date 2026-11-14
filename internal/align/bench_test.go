package align

import (
	"testing"

	"dashpack/internal/ingest"
)

func BenchmarkPipeline(b *testing.B) {
	tracks, err := ingest.Synthesize(ingest.GenParams{Seed: 42, Reps: 4, DurationSec: 600, SegSec: 4})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Align(tracks, 4); err != nil {
			b.Fatal(err)
		}
	}
}
