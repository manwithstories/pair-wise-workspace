package pipeline

import "strings"

// stringReader is a test-only adapter from a manifest string to an io.Reader.
func stringReader(s string) *strings.Reader { return strings.NewReader(s) }
