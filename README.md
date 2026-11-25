# mediad — 区域直播跨节点媒体分发实验台

纯本地 Go 1.25.6 项目。样例 H.264/H.265/AV1 裸码流、逐 AU 索引和 48 kHz 双声道
S16LE PCM 均由代码确定性生成，不读取外部媒体文件，不联网，不转码，不打开图形界面。

## 快速验收

```bash
go mod download
go mod tidy
go build ./...
go test ./...

OUT="$(mktemp -d)" && go run ./cmd/mediad demo \
  --out "$OUT" --codec all --seed 41 --target-bps 900000 \
  --drift-ppm 120 --inject wrap,duplicate,truncate --resume
```

也可运行：

```bash
./scripts/demo.sh
./scripts/check_exit_codes.sh
```

演示依次输出：

1. `[normal]`：三种编码的正常样例，33 位 PTS 回绕、120 ppm 漂移校正、RTP 去重、
   静音检测、关键帧切片、指纹去重和 fMP4/清单哈希。
2. `[corrupt-isolated]` / `[isolation]`：一个截断或越界 AU 被隔离，报告首个绝对
   错误字节；后续 AU 仍使用原绝对偏移，不静默平移时间轴。
3. `[uninterrupted]`、`[interrupted]`、`[resumed]`、`[recovery]`：模拟一个片段
   提交后的中断，再从原子检查点恢复；恢复清单与无中断运行的 SHA-256 一致。

终端字段包括 `target_bps`、`actual_bps`、`delta_pct`、`complete_au_pct`、`bad_au`、
`keyframe_coverage_pct`、`align_max_ms`、`silence_duration_ms`、
`duplicate_saved_bytes` 和 `manifest_sha256`。

## 退出码

| 码 | 含义 |
|---:|---|
| 0 | 正常、损坏隔离和恢复均成功 |
| 2 | 命令行参数错误 |
| 3 | 不可恢复输入，例如超过 2 秒没有可用关键帧 |
| 4 | 内部、文件、原子写入或检查点损坏错误 |

注意：通过 `go run` 传播子进程退出码受 Go 工具自身行为影响；验收稳定退出码时请先
`go build -o <临时路径>/mediad ./cmd/mediad` 再运行二进制。
`scripts/check_exit_codes.sh` 已按此方式验证。

## 模块

- `internal/timeline`：纳秒基准、33 位 PTS 展开、≤500 ppm 漂移校正、带 AU/字节偏移的错误类型。
- `internal/transport`：pion/rtp 封包/解析、序号展开、排序、去重、512 包或 250 ms 有界窗口。
- `internal/codec`：H.264 Annex B、H.265 NAL、AV1 OBU 边界和索引一致性校验。
- `internal/silence`：48 kHz 双声道 S16LE、20 ms RMS、-48 dBFS、500 ms 持续静音和未知缺样区间。
- `internal/cut`：仅从有效关键帧开始切片，无关键帧时延后，超过 2 秒失败并保留已提交片段。
- `internal/dedup`：编码配置 + AU 内容 SHA-256，忽略时间戳；哈希命中后逐字节复核。
- `internal/checkpoint`：原子临时文件提交、路径限制、完整片段日志、损坏检查点显式失败。
- `internal/pack`：go-mp4 v1.7.3 生成并递归校验 fMP4 初始化段和媒体片段。
- `internal/pipeline`：有界队列、64 MiB 内存预算、8 MiB 单 AU 上限和确定性清单。
- `internal/sample`：确定性夹具生成器。
- `cmd/mediad`：纯终端入口和演示输出。

## 输出目录

每个编码场景包含：

- `init-<codec>.mp4`：fMP4 初始化段；
- `seg-<codec>-NNN.m4s`：关键帧对齐媒体片段；
- `manifest-<codec>.json`：片段、时间对齐和质量统计；
- `journal-<codec>.json`：恢复检查点，只记录完整且哈希确定的片段。

所有结果文件均通过同目录临时文件、`fsync` 和原子 `rename` 提交。
