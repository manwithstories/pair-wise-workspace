package report

import (
	"encoding/json"
	"math"
)

type SafeFloat float64

func (s SafeFloat) MarshalJSON() ([]byte, error) {
	value := float64(s)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return []byte("null"), nil
	}
	return json.Marshal(value)
}
