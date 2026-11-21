package fixture

import (
	"testing"
)

func TestCorruptKindsProduceDistinctBytes(t *testing.T) {
	base := DefaultPlan(2, 8).Build()
	for _, kind := range []Kind{KindTruncatedBox, KindCorruptStbl, KindTailTruncation, KindPayloadFlip} {
		out, dmg, err := Corrupt(base, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if string(out) == string(base) {
			t.Fatalf("%s did not change bytes", kind)
		}
		if dmg.LocatorTarget < 0 {
			t.Fatalf("%s has no locator", kind)
		}
	}
}
