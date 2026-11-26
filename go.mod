module mediapipe

go 1.25.6

toolchain go1.25.6

require github.com/asticode/go-astiav v0.40.0

require github.com/asticode/go-astikit v0.42.0 // indirect

// go-astiav v0.40.0 targets FFmpeg 8 and cannot parse against FFmpeg 9's
// removed codec identifiers. v0.43.0 keeps the same core demux/mux API and the
// required go-astikit v0.42.0 dependency while supporting FFmpeg 9 headers.
replace github.com/asticode/go-astiav => github.com/asticode/go-astiav v0.43.0
