package ingest

// Test-only accessors so the fixture generator in testdata/ can emit the
// exact SMPTE-style labels the MXF parser recognizes without duplicating the
// 16-byte constants (which would let the two drift apart).

// MXFPartitionULForTest returns the partition pack universal label.
func MXFPartitionULForTest() []byte { return mxfPartitionUL[:] }

// MXFVideoULForTest returns the video essence universal label.
func MXFVideoULForTest() []byte { return mxfVideoEssenceUL[:] }

// MXFAudioULForTest returns the audio essence universal label.
func MXFAudioULForTest() []byte { return mxfAudioEssenceUL[:] }
