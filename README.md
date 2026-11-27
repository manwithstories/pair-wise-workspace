# avcore 离线媒体处理内核

竞技游戏团队的低延迟互动音视频离线处理内核：统一整数时基、可恢复的
HLS 多码率分片、字幕切片与量化核验。纯本地运行，无外部服务依赖。

## 模块结构

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 共享 90 kHz 整数时基；24/25/30/30000-1001 帧率与 SMPTE 丢帧换算、负偏移、日界回绕 |
| `internal/caption` | 流式 SRT/WebVTT 解析；坏记录带行号/字节偏移/错误码，仅跳过不可恢复项 |
| `internal/adapter` | 版本化 ffprobe/ffmpeg 适配器；结构化错误、安全路径、PSNR/SSIM 度量 |
| `internal/segment` | 共同关键帧切点规划；所有码率层共享切点，字幕按同一时基切片 |
| `internal/state` | 任务指纹、临时文件→SHA256 校验→原子提交、损坏/缺片检查点隔离 |
| `internal/manifest` | 多码率主清单、LL-HLS 部分分片、分片字幕清单（只读已提交分片） |
| `internal/audit` | 目标/实际码率、PSNR/SSIM、对齐误差与 JSONL 审计 |
| `internal/demo` | 本地合成正常/损坏字幕、时间码回绕与中断恢复夹具 |
| `internal/pipeline` | 串联全链路的可恢复引擎 |
| `cmd/avcore` | 终端命令适配器 |

## 构建与验收

```sh
brew install ffmpeg
go mod tidy
go mod vendor
go build -mod=vendor -o "$TMPDIR/avcore" ./cmd/avcore
go test -mod=vendor ./...
"$TMPDIR/avcore" selftest --out "$TMPDIR/avcore-check" --seed 41 \
  --duration 8s --segment 1s --bitrates 450000,900000
```

### 关于 `toolchain` 指令的环境说明

规格要求 `go.mod` 声明 `go 1.25.6` 与 `toolchain go1.25.6`。提交的
`go.mod` 含这两行。在已安装 Go 1.25.6 且 `GOTOOLCHAIN=local` 的机器上，
`go mod tidy` 会按 Go 工具链的标准行为移除与 `go` 指令完全相同的
**冗余** `toolchain` 行（因为当前工具链已满足该 go 指令，无需额外切换）。
该剥离不影响任何功能：剥离后的 `go.mod` 仍声明 `go 1.25.6` 与固定的
`github.com/Eyevinn/hls-m3u8 v0.6.5`，随后的 `go mod vendor`、
`go build -mod=vendor`、`go test -mod=vendor ./...` 全部通过。
这是 Go 1.21+ 工具链对冗余 toolchain 指令的固有处理，无法通过环境变量
或标志绕过；保留一个更高版本号的 toolchain 虽可避免剥离，但会偏离规格
明确指定的 `go1.25.6`，故不采用。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 核验通过 |
| 1 | 内部错误 |
| 2 | 参数错误 |
| 3 | 输入不可恢复 |
| 75 | 注入中断 |

## 设计要点

- **零浮点累计**：所有时间为 int64 的 90 kHz tick；帧率以有理数表示，
  仅在 ffmpeg 命令边界转换为秒。
- **码率口径**：`actual_bps` 取 ffprobe 汇总的视频基本流码率（剔除
  fMP4 封装开销），与 `-b:v` 目标同口径；短分片使用 CBR NAL-HRD 与
  紧凑 VBV 把偏差收敛到 15% 以内。
- **确定性**：单线程 x264 + 剥离容器元数据，相同输入产出字节一致分片，
  因此恢复后的清单摘要与完整重跑严格相等。
- **内存**：分片逐个处理、哈希流式计算、字幕带缓冲流式解析，常驻内存
  与素材时长无关，满足两小时素材 ≤256 MB。
