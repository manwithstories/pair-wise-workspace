// Package scan 实现两阶段流程的第一阶段：扫描输入、计算摘要、生成索引。
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

// DigestFile 计算单个文件的 SHA-256（流式，不缓存整文件）。
func DigestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// DigestBytes 计算内存数据的 SHA-256。
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CombineDigests 把多输入摘要有序绑定为一个总摘要。
// 任一输入变化都会改变总摘要。
func CombineDigests(entries map[string]string) string {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s:%s\n", k, entries[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
