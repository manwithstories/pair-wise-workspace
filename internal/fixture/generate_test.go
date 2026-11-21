package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateIsByteStable(t *testing.T) {
	opts := Options{Seed: 17, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23}}
	a, err := Generate(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ name, x, y string }{
		{"studio", a.StudioMP4, b.StudioMP4},
		{"camera", a.CameraMXF, b.CameraMXF},
		{"rtp", a.RTPStream, b.RTPStream},
	} {
		da, _ := os.ReadFile(pair.x)
		db, _ := os.ReadFile(pair.y)
		if !bytes.Equal(da, db) {
			t.Fatalf("%s differs between identical runs", pair.name)
		}
	}
}

func TestGenerateKeepsFixturesSmall(t *testing.T) {
	b, err := Generate(t.TempDir(), Options{Seed: 17, VFR: []int64{24, 25, 23}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{b.StudioMP4, b.CameraMXF, b.RTPStream} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() >= 8<<20 {
			t.Fatalf("%s is %d bytes, must stay under 8 MiB", name, info.Size())
		}
	}
}

func TestSeedChangesOutput(t *testing.T) {
	a, _ := Generate(t.TempDir(), Options{Seed: 1, VFR: []int64{24}})
	b, _ := Generate(t.TempDir(), Options{Seed: 2, VFR: []int64{24}})
	da, _ := os.ReadFile(a.StudioMP4)
	db, _ := os.ReadFile(b.StudioMP4)
	if bytes.Equal(da, db) {
		t.Fatal("different seeds produced identical fixtures")
	}
}

func TestSidecarHashesMatchContent(t *testing.T) {
	dir := t.TempDir()
	b, err := Generate(dir, Options{Seed: 17, VFR: []int64{24, 25, 23}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	for _, f := range sc.Files {
		data, err := os.ReadFile(filepath.Join(b.Dir, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != f.Hash {
			t.Fatalf("%s sidecar hash %s != %s", f.Name, f.Hash, got)
		}
	}
}

func TestVFRCadenceVaries(t *testing.T) {
	durs := cadenceTicks([]int64{24, 25, 23}, 12, 90000)
	if len(durs) != 12 {
		t.Fatalf("ticks=%d", len(durs))
	}
	deltas := map[int64]bool{}
	for i := 1; i < len(durs); i++ {
		deltas[durs[i]-durs[i-1]] = true
	}
	if len(deltas) < 3 {
		t.Fatalf("VFR cadence produced only %d distinct deltas", len(deltas))
	}
}

func TestPlanDeliveryMovesOneFrameLate(t *testing.T) {
	plan := PlanDelivery(40, 10)
	// Every frame appears exactly once; the duplicate is a separate field and
	// is emitted a second time by the packet writer.
	if len(plan.Order) != 40 {
		t.Fatalf("order len=%d want 40", len(plan.Order))
	}
	seen := map[int]bool{}
	for _, idx := range plan.Order {
		if seen[idx] {
			t.Fatalf("frame %d appears twice in the delivery plan", idx)
		}
		seen[idx] = true
	}
	if plan.Duplicate < 0 {
		t.Fatal("delivery plan has no duplicate")
	}
	// Exactly one frame must be displaced from its natural position: it
	// arrives after frame 30 instead of frame 19.
	pos := map[int]int{}
	for i, idx := range plan.Order {
		pos[idx] = i
	}
	if pos[20] <= pos[30] {
		t.Fatalf("frame 20 at position %d is not late (frame 30 at %d)", pos[20], pos[30])
	}
	displaced := 0
	for idx, p := range pos {
		if idx < 20 && p > idx {
			displaced++
		}
	}
	if displaced != 0 {
		t.Fatalf("earlier frames out of order: %d", displaced)
	}
}
