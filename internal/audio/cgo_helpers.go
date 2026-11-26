package audio

/*
#cgo pkg-config: libavutil
#include <libavutil/frame.h>
#include <string.h>

// mediamix_copy_frame_payload copies n bytes directly into the first data
// plane of an AVFrame. This bypasses the alignment constraints of
// av_image_copy_to_buffer so arbitrary fragment sizes are supported; the
// destination buffer is allocated by av_frame_get_buffer with enough room.
static void mediamix_copy_frame_payload(AVFrame *f, const void *src, int n) {
	memcpy(f->data[0], src, (size_t)n);
}

// mediamix_read_f32 copies n float32 values straight from the frame's first
// data plane, ignoring linesize padding that Go-level Bytes() would include.
static void mediamix_read_f32(AVFrame *f, float *dst, int n) {
	memcpy(dst, f->data[0], (size_t)n * sizeof(float));
}
*/
import "C"

import (
	"unsafe"

	"github.com/asticode/go-astiav"
)

// copyPayload writes the PCM payload into the source frame's first plane.
func copyPayload(f *astiav.Frame, payload []byte) {
	if len(payload) == 0 {
		return
	}
	C.mediamix_copy_frame_payload(
		(*C.AVFrame)(unsafe.Pointer(f.UnsafePointer())),
		unsafe.Pointer(&payload[0]),
		C.int(len(payload)),
	)
}

// readF32 copies n float32 samples from the frame's first plane.
func readF32(f *astiav.Frame, n int) []float32 {
	out := make([]float32, n)
	if n == 0 {
		return out
	}
	C.mediamix_read_f32(
		(*C.AVFrame)(unsafe.Pointer(f.UnsafePointer())),
		(*C.float)(unsafe.Pointer(&out[0])),
		C.int(n),
	)
	return out
}
