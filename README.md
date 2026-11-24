# mediq

本地、确定性的多路 RTP/RTCP 音频汇集、时间线校正、响度/重采样、LPCM-in-MP4
封装与可恢复清单演示。所有样例输入均由 Go 程序在本地生成，不访问外部音频服务
或运行时素材。

## 环境与验收

```bash
brew install go
go install golang.org/dl/go1.25.6@latest
"$(go env GOPATH)/bin/go1.25.6" download

GOVER="$(go env GOPATH)/bin/go1.25.6"
"$GOVER" mod tidy
"$GOVER" build ./...
"$GOVER" test ./...

"$GOVER" run ./cmd/mediq demo --case clean   --out "$TMPDIR/mediq-clean"
"$GOVER" run ./cmd/mediq demo --case damaged --out "$TMPDIR/mediq-damaged"

"$GOVER" run ./cmd/mediq demo --case recovery --crash-after checkpoint \
  --out "$TMPDIR/mediq-recovery"
# 程序自身输出 exit_code=75；Go 工具的 `go run` 按官方文档会把非零子进程
# 状态归并为 1。要核验交付契约中的 shell 75，请使用编译产物：
"$GOVER" build -o /tmp/mediq ./cmd/mediq
/tmp/mediq demo --case recovery --crash-after checkpoint --out "$TMPDIR/mediq-recovery"
"$GOVER" run ./cmd/mediq demo --case recovery --resume --out "$TMPDIR/mediq-recovery"
```

也可直接运行 `./scripts/acceptance.sh`；脚本会用编译后的二进制验证 75，并比较
崩溃续跑、重复续跑和无中断运行三者的 MP4 SHA-256。

## 终端指标

每次演示输出稳定的 `key=value` 行，包含：

- `target_bps=1536000`：48 kHz / 立体声 / S16LE。
- `actual_bps`、`deviation_pct`：LPCM 净荷码率与偏差。
- `missing_samples`、`duplicate_packets`。
- `late_replays`、`late_expired`。
- `integrated_lufs`、`true_peak_dbtp`。
- `manifest_error_ms`。
- damaged 样例额外输出 `first_error_offset` / `parse_error_offset`、
  `missing_seq_ranges` 和解析错误文本。
- 完成时输出 `product_sha256`。

当前夹具为 1 秒节目。clean 与 damaged 的码率偏差和清单误差均为 `0`，响度为
`-23.000 LUFS`，真峰值约 `-22.5 ~ -22.9 dBTP`。damaged 样例的首个异常字节偏移为
`48`，并报告 2 个解析异常、1 个重复包、1 次保留期内回溯、1 个超保留期迟到包和
`20-22,70` 缺失序号区间。

## 退出码

| Code | 含义 |
|---:|---|
| 0 | 完成；damaged 输入中的已计数损坏不阻止交付 |
| 1 | 配置、本地 I/O 或检查点绑定错误 |
| 2 | `--strict` 模式拒绝解析损坏、缺失或超期迟到输入 |
| 75 | 注入的、可续跑中断 |

注意：`go run` 的官方说明是 “The exit status of Run is not the exit status of
the compiled binary”；因此直接经 `go run` 触发 75 时，`go` 命令自身返回 1，
但程序输出和编译产物均保持契约中的 75。

## 架构

```
internal/core       统一 48 kHz 媒体时钟、RTP/SR 映射、可分类错误与退出码
internal/source     确定性 L16 音频、RTP/RTCP SR、损坏/重复/乱序夹具生成
internal/ingest     长度前缀增量读取、精确异常偏移、序号扩展与有界重排
internal/timeline   事件时间水位、区间提交、保留检查点与迟到回溯/拒绝
internal/dsp        L16 解码、单声道→立体声、44.1/48→48 kHz、R128 与真峰值
internal/pack       go-mp4 IPCM/pcmC 写入、原子 MP4 替换、检查点对齐 PCM 暂存
internal/recovery   输入/参数摘要、提交位置、临时文件 + rename 原子检查点
internal/manifest   源序号/时间范围、参数、计数、质量、产物哈希清单
internal/pipeline   上述模块的确定性编排与恢复
cmd/mediq           纯终端 demo 适配器
```

## 确定性与恢复

- 输入文件为 `out/input.lenrtp`：4 字节大端长度前缀 + RTP/RTCP 数据报。
- `checkpoint.json` 记录输入 SHA-256、参数 SHA-256、已提交输出样本数、扩展序号
  位置和最终产物哈希。
- 检查点先写同目录临时文件、`fsync`、`chmod`，再 `rename` 原子替换，最后同步目录。
- `audio.pcm` 的追加位置由检查点决定；续跑时会先截断检查点之后的未提交尾部，
  因此重复执行不会重复追加样本。
- MP4 的时间戳、品牌、矩阵和采样表均使用固定值，不写当前时间或随机临时名到产物
  内部；相同音频生成相同 SHA-256。

## 上限

- 单数据报默认上限 8192 字节，硬上限 1 MiB。
- 重排窗口可配置且最大 4096 个序号；演示使用 8/16。
- 时间线检查点保留区间数最大 1000；超期迟到事件只记录并拒绝回写。
- DSP 输出固定为 48 kHz、立体声、S16LE，净荷码率 1,536,000 bps。

## 测试

```bash
"$(go env GOPATH)/bin/go1.25.6" test ./...
```

包含 core、ingest、timeline、dsp、recovery、pack、pipeline 和 CLI 测试。MP4 产物
会使用 `github.com/abema/go-mp4` 反向解析 `ipcm` 与 `pcmC` box。
