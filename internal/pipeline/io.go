package pipeline

import "bytes"

// bytesReader 把内存素材包装为扫描器需要的 io.Reader。
func bytesReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
