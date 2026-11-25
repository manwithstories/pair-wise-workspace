# MediaPack Go 媒体处理内核

面向远程制作团队的纯本地、可复用 Go 内核：处理 RTP/TS 输入、PCR/PTS 回绕、CC 跳变、PSI 换版与 CRC 拒绝、PES 轨道、关键帧切片、PCM S16LE 静音检测、内容指纹/去重，以及可恢复的 WAL 原子提交和 go-mp4 关键帧索引侧车。

样例媒体全部由代码按固定种子生成，不读取外部媒体文件，不依赖外部服务。

## 环境

- macOS（Apple Silicon / Intel）
- Go 1.25.6；`go.mod` 的 `go` 指令固定为 `1.25.6`
- 依赖：
  - `github.com/abema/go-mp4 v1.7.4`
  - `github.com/pion/rtp v1.10.6`

可使用仓库脚本安装指定版本：

```bash
./scripts/install-go-1.25.6.sh
```

## 验收命令

在项目根目录执行：

```bash
export PATH=/usr/local/go/bin:$PATH
GOTOOLCHAIN=local go mod tidy
GOTOOLCHAIN=local go build ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go run ./cmd/mediapack verify \
  --seed 73 \
  --faults pcr-wrap,cc-gap,psi-update,bad-crc,truncated \
  --buffer-packets 4096 \
  --consumer-delay 20ms \
  --crash-after 2 \
  --resume
```

也可以直接运行：

```bash
./scripts/acceptance.sh
```

成功时退出码为 `0`，终端包含：

```text
roundtrip=100.00%
keyframe_hit=100.00%
hash_repeat=100.00%
peak_heap_mib<=32
bad_crc_rejected=1
truncated_committed=0
wal_replayed=1
```

并为每个片段输出 64 位十六进制：

```text
clip-0001 clip_sha256=<64 hex> content_fingerprint=<64 hex> ...
```

退出码约定：

| 退出码 | 含义 |
|---:|---|
| `0` | 核验通过 |
| `2` | 参数错误 |
| `3` | 不可恢复输入，例如不支持的音频编码或时钟不可判定 |
| `4` | WAL、原子提交、哈希恢复等不变量失败 |

## 数据流

```text
RTP/TS 输入
  → 188 字节限长读取与有界 packet queue（背压/可取消）
  → TS 包解析、CC 顺序检测、PCR 33 位回绕扩展
  → PAT/PMT CRC32-MPEG2 校验与一致性版本切换
  → PES 组装、PTS 回绕扩展、轨道编码识别
  → 统一媒体时钟上的 CC gap / invalid interval
  → 关键帧切片与时间区间音频归属
  → PCM S16LE 分窗能量和毫秒级静音阈值
  → clip SHA-256 与去时间戳/传输头的内容指纹
  → WAL prepared/committed + checksum
  → 临时文件原子 rename、恢复校验、孤儿 .tmp 回滚
  → go-mp4 free box 索引侧车
```

## 目录

- `cmd/mediapack/main.go`：纯终端验收适配器和退出码。
- `internal/ingest/rtp.go`：基于 `pion/rtp` 的 RTP 承载 TS 打包/解包。
- `internal/transport/packet.go`：188 字节 TS 解析、CC tracker、有界队列。
- `internal/transport/psi.go`、`psi_registry.go`：MPEG-2 CRC、PAT/PMT、版本一致性和 last-known-good。
- `internal/timeline/clock.go`：33 位 PCR/PTS 回绕、统一时间区间和错误类型。
- `internal/demux/pes.go`：PES 分片重组、PTS 解析、合成关键帧媒体头和编码声明。
- `internal/cut/segment.go`：关键帧切片、关键帧偏移、无效区间禁提、片段哈希。
- `internal/audio/silence.go`：PCM S16LE 对齐校验、窗口均方能量、毫秒静音阈值。
- `internal/fingerprint/content.go`：片段 SHA-256、规范内容指纹、重复来源索引。
- `internal/txn/wal.go`、`commit.go`：JSON-line WAL、校验和、崩溃注入、恢复和临时文件回滚。
- `internal/index/mp4.go`：用 `go-mp4` 写入/读回 MP4 `free` box JSON 关键帧索引。
- `internal/fixture/`：固定种子的正常/故障媒体生成器。
- `internal/pipeline/`：有界队列和各模块接线。
- `internal/verify/`：验收指标、堆峰值采样、不中断/恢复双路径比对。

## 关键保证

### 背压和取消

`transport.Queue` 是固定容量 channel。队列满时 producer 在 `Push` 上阻塞，不丢包、不无限增长；慢消费者由 `--consumer-delay` 模拟。`Push` 和 `Pop` 都监听 `context.Context`，取消后有界退出。相关竞态路径由 `go test -race` 覆盖。

### CC 跳变

CC 判定在消费者按出队顺序执行，避免 producer 预读导致时间区间错位。检测到跳变后只记录 `[last-boundary, current-boundary)`，写入片段和索引 marker，不补造缺失媒体。

### PCR/PTS 回绕

原始 33 位值按模 `2^33` 和半程阈值扩展为单调计数器，再以首个观测值为原点归一到媒体时间。切片和静音检测都使用该 `time.Duration` 时间轴。PCR 超范围或后退到不可判定时返回 `timeline.Error`；与 invalid interval 相交的片段会被标记为不可提交。

### PSI 换版

PSI section 使用 CRC-32/MPEG-2（初值 `0xffffffff`、多项式 `0x04c11db7`、MSB-first、无最终异或）。新版本必须 CRC 正确且两份内容/版本一致后才生效；坏 CRC 增加 `bad_crc_rejected` 并继续使用上一有效 PMT。

### 切片、静音和编码

每个提交片段都从关键帧开始。音频必须是小端 signed 16-bit PCM；非 16-bit/不对齐数据明确报错，PMT 中未知/不支持编码通过统一错误类型报告。静音检测按 20 ms 窗口计算均方能量，最短静音长度由毫秒阈值配置。

### 指纹和重复内容

- `clip_sha256`：提交的长度分帧片段 blob 的 SHA-256。
- `content_fingerprint`：仅对去除 RTP/TS/PES 头和 PTS 偏移后的 elementary payload 做规范化长度分帧哈希。
- 固定种子夹具的第 1、4 片段内容相同，因此哈希一致；provenance 记录两个来源，内容寻址 blob 只落盘一次。

### WAL 和恢复

每个 artifact：

1. 写入同文件系统 `tmp/*.tmp` 并 fsync；
2. WAL 追加 checksummed `prepared`；
3. 原子 `rename` 到 `objects/...`；
4. WAL 追加 checksummed `committed`。

`--crash-after 2` 恰好在第一个片段的 prepared+committed 两条 WAL 后注入崩溃。恢复时：

- 校验 WAL 记录 checksum，撕裂尾部不恢复；
- 对 committed 记录重新读取对象并核对 SHA-256/size；
- 清理 commit 边界外的所有 `*.tmp`；
- 相同输入重跑得到逐项一致的片段哈希。

### 内存

验收进程每秒采样 Go `HeapAlloc`，输出实际值和 `peak_heap_mib<=32` 判定。夹具和队列均为固定小包；默认验收样例的观测峰值通常为数 MiB。

## 说明：合成视频编码

仓库不携带外部媒体，也不实现完整 H.264 解码器。视频轨使用明确命名的 `h264-synthetic` 编码和 `MPCK` 关键帧/序号头来驱动 TS/PES/切片/索引往返；PCM S16LE 静音检测是真实的字节解析和能量计算。PSI、PCR、CC、WAL、RTP 和 MP4 sidecar 均使用真实协议结构或指定依赖。
