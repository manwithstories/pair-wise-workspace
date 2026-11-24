# 广播节目交付流水线（media-pipeline）

纯本地、无外部服务依赖的多声道 PCM/WAV 节目处理流水线。从 RTP/UDP 捕获与本地
WAV 输入出发，完成乱序重排、缺块定位、WAV 校验、采样边界切分、静音检测、内容
指纹、fMP4 无损封装与断点恢复，全部产物可复核、可重复。

## 技术栈

- Go 1.25.6（`go.mod` 的 `go` 指令固定为 `1.25.6`）
- `github.com/abema/go-mp4 v1.3.0`：fMP4 封装与回读（ipcm/pcmC 无损整数 PCM）
- `github.com/pion/rtp v1.8.25`：RTP 头与序号解析
- 其余能力（WAV/PCM、哈希、命令行、测试）均由标准库实现

## 构建、测试与演示

```bash
go mod tidy
go build ./...
go test ./...

# 端到端演示：自生成正常素材、重复乱序包、突发缺包、头部损坏、
# 非整帧尾块与中断续跑六类样例
go run ./cmd/mediad demo --seed 42 --out /tmp/media-demo --queue 4 --resume

# 独立覆盖重排、损坏定位、恢复与封装回读
go test ./internal/ingest ./internal/wav ./internal/resume ./internal/mux -count=1

# 本地捕获文件批处理（仅读取 --in 指定目录下的 .rtpcap）
go run ./cmd/mediad batch --in <本地输入目录> --out <本地输出目录>
```

## 输出口径

逐场景行与汇总行字段：

```
segments=N silence_ms=N duplicates=N gaps=N bad_byte=偏移 bad_sample=采样序号 resume_ms=耗时 sha256=<64 位十六进制>
```

- 正常样例 `gaps=0`；损坏样例字节偏移与预置注入点相差不超过 1 字节。
- `duplicates` 为 RTP 传输层重复包数；内容指纹重复单列于清单 `content_dups`。
- `sha256` 是机器可读清单 `manifest.json`（不含自哈希与墙钟字段）的摘要，
  相同 seed 与参数下重复运行逐字节一致。
- `resume_ms` 仅为恢复阶段墙钟耗时，不参与内容哈希。

退出码：`0` 全部校验通过；`2` 输入损坏或参数错误；
`3` 不可恢复的检查点/校验和冲突；`1` 内部处理失败。

## 架构与数据流

```
RTP 接收/重排（去重、有界窗口、缺口期限）
        │  连续采样块 + 缺口证据（不伪造静音）
        ▼
WAV/PCM 校验（RIFF/fmt/data、块对齐、截断定位）
        │
        ├──► 静音检测（滑动窗口峰值/RMS，跨声道聚合）
        ├──► 内容指纹（规范化 PCM 窗口 SHA-256，跨块边界一致）
        ▼
采样边界切分（时长/静音边界，不重复计时）
        ▼
fMP4 封装与回读核验（ipcm 无损 PCM）◄──► 断点恢复检查点（原子写、身份核对）
```

共享时间基准（`internal/clock`）与结构化错误模型（`internal/fault`）被所有子系统
复用；各子系统均有独立测试。有界通道（`internal/pipeline`）在消费者变慢时阻塞
生产者，队列、重排窗口与在途 PCM 总量均有硬上限（默认缓冲 64 MiB、单块 1 MiB）。

## 约束

- 仅处理整数 PCM、RTP/UDP 捕获文件与本地 WAV；不接真实设备、不做有损编码。
- 输出为分段 fMP4 与机器可读清单，统一走临时文件加原子改名。
- 输入路径限定用户明确给定的本地目录；仓库不存放密钥或外部连接信息。
