# mediascan

本地、确定性的互动直播媒体扫描工具。在统一的整数微秒时基上校验 MP4 与 RTP
轨道的可解码性、关键帧间隔、时钟漂移、32 位时间戳回绕，并支持网络抖动导致
中断后的断点续跑，且恢复不改变结果（损坏位置、归并顺序、SHA-256 完全一致）。

纯终端、无图形界面、无外部服务依赖；样例数据全部由代码按 seed 生成。

## 环境与构建

要求 Go 1.25.6（`go.mod` 的 `go 1.25.6` 固定版本；本地版本更旧时工具链会自动
下载对应版本）。支持 Darwin arm64 与 amd64。

```sh
go mod download
go mod tidy
go build ./...
go test ./...          # 全部单元测试；可加 -race 运行竞态检测
```

## 模块结构

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 1 MHz 整数时基、无浮点有理数换算（含溢出检查）、RTP 32 位时间戳回绕展开 |
| `internal/fault` | 跨子系统统一的轨道错误模型与确定性全序 |
| `internal/mp4` | box 层级/截断扫描、VFR 样本时长、关键帧间隔、go-mp4 交叉校验、夹具生成 |
| `internal/rtp` | RTP 序号连续性、回绕/歧义故障映射（pion/rtp） |
| `internal/metadata` | 字幕/章节/ID3/emsg/SEI 事件归一化与时钟漂移检测 |
| `internal/pipeline` | 有界工作池（队列与并发上限）与确定性归并 |
| `internal/checkpoint` | 原子断点记录、输入/参数摘要校验、续跑跳过 |
| `internal/report` | 规范化序列化与确定性 SHA-256 摘要 |
| `internal/demo` | 确定性多轨样例引擎，贯穿扫描→归并→恢复链路 |
| `cmd/mediascan` | 终端适配器 |

## 演示（验收命令）

```sh
# 第一次运行：处理到 500 帧后取消，检查点可续跑
go run ./cmd/mediascan demo --seed 42 --tracks 4 --frames 1200 \
  --corrupt-at 731 --checkpoint ./tmp/checkpoint.json --cancel-after 500

# 第二次运行：从检查点恢复，仅重算剩余 700 帧
go run ./cmd/mediascan demo --seed 42 --tracks 4 --frames 1200 \
  --corrupt-at 731 --checkpoint ./tmp/checkpoint.json --resume
```

输出契约（key=value）：

- 正常：`tracks=4 frames=1200`
- 损坏：`first_bad_frame=731 offset_bytes=<数值> localization_error_frames=0`
- 恢复：`resume_ms=<数值> recomputed_frames<=700 sha256=<64 位十六进制>`
- 两次完整运行（直跑 与 取消+恢复）的 `sha256` 完全相同；1200 帧恢复耗时 < 2s

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 完成 |
| 2 | 输入损坏但已定位 |
| 3 | 参数或时基无效 |
| 4 | 取消，检查点可续跑 |
| 5 | 检查点不匹配或输出校验失败 |

> **关于 `go run` 的退出码**：Go 工具链的 `go run` 会把程序的任意非零退出码
> 归一化为 `1`（仅打印 `exit status N`），因此无法通过 `go run` 观察到 2/3/4/5。
> 程序本身通过 `os.Exit` 返回的语义码是正确的。需要精确退出码时，请先构建再
> 运行，或使用透传退出码的包装脚本：
>
> ```sh
> go build -o ./tmp/mediascan ./cmd/mediascan
> ./tmp/mediascan demo ...            # 退出码精确为 0/2/3/4/5
> # 或
> ./scripts/run-demo.sh demo ...      # 内部构建并 exec，透传真实退出码
> ```

## 限制

文件大小、box 深度、轨道数、样本数、队列长度与最大并发均有硬上限（见各包的
`Limits`/`DefaultLimits`），超限输入被拒绝；所有时基换算为带溢出检查的整数
运算，避免浮点累计误差与无界内存增长。交付仅含源码、测试与脚本，不含构建产物。
