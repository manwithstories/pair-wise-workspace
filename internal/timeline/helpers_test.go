package timeline

import "bytes"

// byteReader renders a fixture document for decoder tests.
func byteReader(d fixtureBuilder) *bytes.Reader {
	return bytes.NewReader(d.Build())
}

// fixtureBuilder is the subset of fixture.Document used by the tests.
type fixtureBuilder interface {
	Build() []byte
}
