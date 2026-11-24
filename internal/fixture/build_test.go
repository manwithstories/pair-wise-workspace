package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildAllCases(t *testing.T) {
	for _, name := range AllCases {
		t.Run(name, func(t *testing.T) {
			tree, err := Build(t.TempDir(), name)
			if err != nil {
				t.Fatal(err)
			}
			if tree.DASHMPD == "" || tree.HLSMaster == "" || tree.RTPRecord == "" {
				t.Fatalf("tree missing artifacts: %+v", tree)
			}
			if len(tree.InitFiles) != 3 || len(tree.FragmentDir) != 3 {
				t.Fatalf("expected 3 reps, got init=%d dirs=%d",
					len(tree.InitFiles), len(tree.FragmentDir))
			}
			// Each representation must have init + 24 fragments + media playlist.
			for _, spec := range Layout {
				dir := tree.FragmentDir[spec.ID]
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				var m4s, m3u8 int
				for _, e := range entries {
					switch filepath.Ext(e.Name()) {
					case ".m4s":
						m4s++
					case ".m3u8":
						m3u8++
					}
				}
				if m4s != SegmentCount {
					t.Fatalf("case %s rep %s fragments=%d want %d", name, spec.ID, m4s, SegmentCount)
				}
				if m3u8 != 1 {
					t.Fatalf("rep %s media playlist count=%d", spec.ID, m3u8)
				}
			}
		})
	}
}

func TestUnknownCaseRejected(t *testing.T) {
	if _, err := Build(t.TempDir(), "nope"); err == nil {
		t.Fatal("unknown case must error")
	}
}

func TestDeterministicFragmentBytes(t *testing.T) {
	root1, root2 := t.TempDir(), t.TempDir()
	t1, err := Build(root1, CaseClean)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := Build(root2, CaseClean)
	if err != nil {
		t.Fatal(err)
	}
	spec := Layout[1]
	for seq := 1; seq <= SegmentCount; seq++ {
		a, err := os.ReadFile(filepath.Join(t1.FragmentDir[spec.ID], segFileName(seq)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(t2.FragmentDir[spec.ID], segFileName(seq)))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("fragment %d not deterministic", seq)
		}
	}
}

func TestFaultsChangeCapture(t *testing.T) {
	cleanTree, err := Build(t.TempDir(), CaseClean)
	if err != nil {
		t.Fatal(err)
	}
	cleanCapture, err := os.ReadFile(cleanTree.RTPRecord)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{CaseDuplicate, CaseReorder, CaseBurstLoss} {
		tree, err := Build(t.TempDir(), name)
		if err != nil {
			t.Fatal(err)
		}
		capBytes, err := os.ReadFile(tree.RTPRecord)
		if err != nil {
			t.Fatal(err)
		}
		if string(capBytes) == string(cleanCapture) {
			t.Fatalf("case %s did not mutate the capture", name)
		}
	}
}

func TestTruncatedBoxShortensFiles(t *testing.T) {
	cleanTree, _ := Build(t.TempDir(), CaseClean)
	badTree, err := Build(t.TempDir(), CaseTruncatedBox)
	if err != nil {
		t.Fatal(err)
	}
	spec := Layout[0]
	for _, seg := range []int{faultSegA, faultSegB} {
		good, _ := os.ReadFile(filepath.Join(cleanTree.FragmentDir[spec.ID], segFileName(seg)))
		bad, err := os.ReadFile(filepath.Join(badTree.FragmentDir[spec.ID], segFileName(seg)))
		if err != nil {
			t.Fatal(err)
		}
		if len(bad) >= len(good) {
			t.Fatalf("segment %d not truncated: %d >= %d", seg, len(bad), len(good))
		}
	}
}
