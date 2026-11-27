package mapping

import "os"

// writeFile 是测试用的小工具，集中一处避免在每个用例里重复 os 调用。
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
