package index

import (
	"math/rand"
	"os/exec"
	"strings"
	"testing"
)

// TestMP4ParsableByFFprobe confirms the MOV/MP4 demuxer opens the generated
// container and reads its sample table and stream metadata. Synthetic samples
// are not decodable H.264; the probe is container-level only.
func TestMP4ParsableByFFprobe(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	dir := t.TempDir()
	rep := buildReport(t, 200)
	idx, faults := Build(rep, dir, 25)
	if len(faults) > 0 {
		t.Fatal(faults)
	}
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_name,nb_frames:format=nb_streams,format_name",
		"-of", "default=noprint_wrappers=1", idx.File).CombinedOutput()
	t.Logf("ffprobe:\n%s", out)
	if err != nil {
		t.Fatalf("ffprobe rejected the container: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "codec_type=video") || !strings.Contains(text, "mov") {
		t.Fatalf("container metadata missing:\n%s", text)
	}
	checked, sf := RandomSeeks(idx, 100, rand.New(rand.NewSource(1)))
	if len(sf) > 0 || checked != 100 {
		t.Fatalf("random seeks checked=%d faults=%v", checked, sf)
	}
}
