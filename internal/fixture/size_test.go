package fixture

import "testing"

func TestDefaultPlanSize(t *testing.T) {
	b := DefaultPlan(3, 240).Build()
	if len(b) < 10<<20 {
		t.Fatalf("default plan should be >= 10MB, got %d", len(b))
	}
	t.Logf("size=%d", len(b))
}
