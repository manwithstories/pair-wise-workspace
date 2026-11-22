# mediad — 本地 HLS/DASH 分片检查与再封装验收工具

`mediad` 是一个可复用的 Go 媒体处理库，命令行只负责驱动验收与批处理。
所有样例均在本地生成，不访问网络。

## 构建与验收

```sh
brew install go
go mod download
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediad demo --seed 42 --chunk-size 8 --workers 4 --resume
go run ./cmd/mediad verify --input <生成目录>
```

`demo` 默认在临时目录生成五组样例；使用 `--dir` 指定固定位置后，`verify`
可以直接复核该目录。退出码：0 全部阈值通过，1 验收指标未通过，2 参数或输入错误。

## 样例

| 目录 | 内容 |
| --- | --- |
| `normal` | 正常分片，往返与关键帧命中应为 100% |
| `truncated` | 一个分片尾部被截断，产生时间线缺口 |
| `corrupt_box` | 一个 moof 的 box 长度被篡改，越界偏移被精确记录 |
| `missing_index` | 不提供 `keyframes.json`，强制按分片扫描重建索引 |
| `timestamp_jump` | 中点之后的分片整体前移，形成跨分片时间戳跳变 |

每组目录包含 `init.mp4`、`segNNN.m4s`、`index.m3u8`、`manifest.mpd`、
可选的 `keyframes.json`、检查点目录 `checkpoint/`、规范化输出 `out/`、
`rtp_evidence.json` 与批次报告 `report.json`。

## 模块

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 统一 90 kHz 媒体时间基准，精确的十进制/ISO-8601 解析 |
| `internal/fault` | 带分片位置与偏移的结构化错误 |
| `internal/manifest` | HLS 与 DASH 解析、统一分片模型与往返序列化 |
| `internal/probe` | fMP4 box 边界、moof/mdat、截断与时间戳检查 |
| `internal/index` | 关键帧索引、缺失时按分片重建、命中率分母 |
| `internal/audio` | 48 kHz/立体声/响度规范化，PCM 解码与交付字节编码 |
| `internal/worker` | 有界并发流水线，固定分块与确定性合并 |
| `internal/checkpoint` | 原子检查点、输入摘要绑定与恢复拒绝 |
| `internal/hash` | 逐片段 SHA-256 证据与比对 |
| `internal/rtp` | 分段时间戳与 RFC 6464 音频电平证据 |
| `internal/fixture` | 本地样例生成与路径越界拒绝 |
| `cmd/mediad` | `demo` 与 `verify` 入口 |

## 设计要点

- **单一时间基准**：清单、探测、索引、音频与检查点均以 90 kHz tick 交换，
  同时保留原生时基与序号用于精确往返。
- **不伪造数据**：无法确定时长或无法解码时保留时间线缺口并记录故障，
  不填充静音或猜测时长。
- **可证明的恢复**：检查点记录清单摘要与逐片段输出哈希；恢复结果必须与
  独立全量重算逐段一致，否则验收失败。
- **确定性**：相同输入重复运行得到相同的清单、索引、片段哈希与 RTP 证据。
- **有界资源**：并发数受信号量限制，box 扫描与分块大小均有上限，
  所有 URI 经 `SafeJoin` 校验，拒绝绝对路径与 `..` 越界。
