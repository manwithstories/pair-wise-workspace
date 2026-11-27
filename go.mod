module tscheck

go 1.25.6

require github.com/asticode/go-astits v1.16.0

require github.com/asticode/go-astikit v0.30.0 // indirect

// Pinned toolchain: go1.25.6 (equal to the go directive; the go tool
// drops a redundant toolchain line on tidy, so the pin is carried by
// the go directive above. Do not change the Go version.)
