package pipeline

import (
	"encoding/json"
)

// jsonEncode serializes any stage artifact deterministically. Only
// ordered structs and sorted slices are stored, never maps with
// iteration order.
func jsonEncode(v any) ([]byte, error) {
	return json.Marshal(v)
}
