package fixture

import "bytes"

// byteBuffer is a small growable writer adapter around bytes.Buffer.
type byteBuffer struct{ b bytes.Buffer }

func (w *byteBuffer) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w *byteBuffer) bytes() []byte               { return w.b.Bytes() }

func byteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
