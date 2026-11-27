module archivemedia

// 固定使用 Go 1.25.6。不额外书写 toolchain go1.25.6 指令：在已安装
// Go 1.25.6 且 GOTOOLCHAIN=local 的环境下，与当前工具链同名的冗余 toolchain
// 行会被 `go build`/`go mod tidy` 判定为需清理项而直接报错，go 指令本身
// 已足以把版本锁定在 1.25.6。
go 1.25.6

require github.com/asticode/go-astiav v0.43.0

require github.com/asticode/go-astikit v0.42.0 // indirect
