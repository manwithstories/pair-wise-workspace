package pipeline

import (
	"github.com/mediamend/mediamend/internal/model"
	"testing"
)

func TestCycleMapsExit3(t *testing.T) {
	faults := []model.Fault{{Class: model.FaultGraphCycle, Severity: model.SeverityFatal}}
	if code := model.ExitCode(faults); code != 3 {
		t.Fatalf("graph cycle exit=%d want 3", code)
	}
}
