package parse

import "errors"

// FindTop returns the first top-level box of the given type.
func FindTop(boxes []RawBox, typ string) *RawBox {
	for i := range boxes {
		if boxes[i].String() == typ {
			return &boxes[i]
		}
	}
	return nil
}

// ChildrenOf returns the children of the first child with the given type.
func ChildrenOf(boxes []RawBox, typ string) []RawBox {
	for i := range boxes {
		if boxes[i].String() == typ {
			return boxes[i].Children
		}
	}
	return nil
}

// Walk visits every box depth-first.
func Walk(boxes []RawBox, fn func(RawBox) error) error {
	for _, b := range boxes {
		if err := fn(b); err != nil {
			return err
		}
		if err := Walk(b.Children, fn); err != nil {
			return err
		}
	}
	return nil
}

// CollectUnknown returns every leaf box whose type the reader does not
// understand, in file order.
func CollectUnknown(boxes []RawBox) []RawBox {
	var out []RawBox
	Walk(boxes, func(b RawBox) error {
		name := b.String()
		if !containerTypes[name] && !knownLeafTypes[name] {
			out = append(out, b)
		}
		return nil
	})
	return out
}

var ErrUnknownBox = errors.New("parse: unknown box type")
