package ebml

import "mediaproc/internal/model"

// DecodeVINT decodes one VINT from an in-memory byte slice. It is useful for
// block headers and metadata payloads.
func DecodeVINT(buf []byte, asID bool) (VINT, int, error) {
	if len(buf) == 0 {
		return VINT{}, 0, model.NewError(model.PhaseEBML, 0, true, "empty VINT", nil)
	}
	first := buf[0]
	if first == 0x00 {
		return VINT{Length: 1}, 1, model.NewError(
			model.PhaseEBML, 0, true, "illegal VINT: all-zero length descriptor", nil)
	}
	length := 1
	for mask := byte(0x80); first&mask == 0; mask >>= 1 {
		length++
		if length > MaxVINTBytes || length > len(buf) {
			return VINT{Length: length}, length, model.NewError(
				model.PhaseEBML, 0, true, "truncated or illegal VINT", nil)
		}
	}
	if length > len(buf) {
		return VINT{Length: length}, length, model.NewError(
			model.PhaseEBML, 0, true, "truncated VINT", nil)
	}
	var raw uint64
	for i := 0; i < length; i++ {
		raw = raw<<8 | uint64(buf[i])
	}
	v := VINT{Raw: raw, Value: raw, Length: length}
	if !asID {
		marker := uint64(1) << uint(7*length)
		v.Value = raw &^ marker
		v.Unknown = v.Value == marker-1
	}
	return v, length, nil
}
