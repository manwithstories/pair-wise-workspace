package checkpoint

import (
	"bytes"
	"os"
)

func osReadFile(p string) ([]byte, error)  { return os.ReadFile(p) }
func osWriteFile(p string, b []byte) error { return os.WriteFile(p, b, 0o644) }

// replaceFirst 替换字节切片中首个匹配子串，找不到时原样返回。
func replaceFirst(b []byte, old, new string) []byte {
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}
