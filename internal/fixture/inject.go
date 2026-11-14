package fixture

import (
	"os"
)

// InjectUnknownBoxes appends free-form unknown top-level boxes to exercise the
// scanner's skip-and-count behavior without affecting the playable structure.
func InjectUnknownBoxes(path string, count int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var add []byte
	for i := 0; i < count; i++ {
		payload := []byte{0xAA, 0xBB, 0xCC, 0xDD}
		add = append(add, box("zz99", payload)...)
	}
	return os.WriteFile(path, append(b, add...), 0o644)
}
