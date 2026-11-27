package pipeline

import "bytes"

// bytesReader 是测试辅助：把夹具字节包装为 ReadSeeker。
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
