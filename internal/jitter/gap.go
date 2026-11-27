package jitter

// Gap 描述一个检测到的序列号缺口及其“年龄”（与最新报文的距离）。
type Gap struct {
	Track uint32
	Seq   int64
	Age   int64
}
