package color

import "testing"

func TestResolveNoConflict(t *testing.T) {
	cam := Metadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited", Source: "camera-config"}
	bs := Metadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited", Source: "bitstream"}
	out, conflicts := Resolve(cam, bs)
	if len(conflicts) != 0 {
		t.Fatalf("conflicts=%v", conflicts)
	}
	if out.Primaries != "bt709" || out.Source != "camera-config" {
		t.Fatalf("out=%+v", out)
	}
}

func TestResolveConflictCameraWins(t *testing.T) {
	cam := Metadata{Primaries: "bt709", Range: "limited", Source: "camera-config"}
	bs := Metadata{Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc", Range: "full", Source: "bitstream"}
	out, conflicts := Resolve(cam, bs)
	if out.Primaries != "bt709" || out.Range != "limited" {
		t.Fatalf("camera config must win: %+v", out)
	}
	if out.Transfer != "pq" || out.Matrix != "bt2020nc" {
		t.Fatalf("bitstream should fill unset fields: %+v", out)
	}
	if len(conflicts) != 2 {
		t.Fatalf("conflicts=%v", conflicts)
	}
	for _, c := range conflicts {
		if c.Resolved != c.Camera || c.Reason == "" {
			t.Fatalf("conflict missing provenance: %+v", c)
		}
	}
}

func TestResolveDefaults(t *testing.T) {
	out, conflicts := Resolve(Metadata{}, Metadata{})
	if len(conflicts) != 0 {
		t.Fatalf("conflicts=%v", conflicts)
	}
	if out.Primaries != "bt709" || out.Range != "limited" || out.Source != "default" {
		t.Fatalf("out=%+v", out)
	}
}
