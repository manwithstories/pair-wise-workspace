# mediaverify

广播交付批量音频校验工具：对多制作方提交的节目统一音频规格、控制响度、
重新封装，并以机器可读数据证明输出码率与播放清单时间线符合交付约定。
纯本地运行，无网络与外部服务依赖；样例数据随代码生成。

## 构建与测试（macOS）

```sh
brew install go@1.23
export PATH="$(brew --prefix go@1.23)/bin:$PATH"
go mod tidy
go build ./...
go test ./...
```

## 演示

```sh
go run ./cmd/mediaverify demo --seed 42 --target-kbps 256 --rate 48000 \
  --channels 2 --loudness -16 --loss-burst 4 --bandwidth-kbps 900
```

输出三行 JSON：

1. `normal`：正常样例。生成夹具 → 扫描建索引 → 流式转换。
   码率偏差 ≤10%、时间对齐误差 ≤20ms，退出码 0。
2. `corrupt`：损坏样例（截断分段）。给出带文件与段位置的可定位错误。
3. `resumed`：恢复样例。第 2 段后模拟中断，续跑时校验输入摘要，
   从已完成分段边界继续（`recovery_action=resumed`，`resumed_from_segment=2`）。

字段：`target_kbps`、`actual_kbps`、`bitrate_error_pct`、`input_lufs`、
`output_lufs`、`peak_dbfs`、`silence_ratio`、`gap_packets`、
`duplicate_packets`、`reordered_packets`、`manifest_error_ms`、
`index_digest_match`、`recovery_action`、`resumed_from_segment`。

退出码：`0` 成功；`2` 错误（输入损坏、摘要 mismatch、时间线无歧义映射失败等）；
`3` 质量门限失败（码率偏差 >10%、时间对齐误差 >20ms、峰值超限）。

## 子命令

- `mediaverify demo`：端到端演示（夹具生成 + 三场景）。
- `mediaverify scan --manifest prog.mpd --dir . --index index.json`：仅扫描建索引。
- `mediaverify run --manifest prog.mpd --dir . --index index.json --out out.mp4
  [--checkpoint ck.json --resume] [--loss-stop]`：仅流式执行。

## 架构

两阶段流程：**扫描建索引**（`internal/scan`，索引绑定全部输入摘要，
任一输入变化即拒绝旧索引并要求重扫）→ **流式执行**（`internal/run`，
分块处理，不缓存整节目）。

| 模块 | 职责 |
| --- | --- |
| `internal/clock` | 统一纳秒时间基准；discontinuity 显式时钟重置映射 |
| `internal/fault` | 带文件/行/段位置与恢复策略的错误模型 |
| `internal/manifest` | DASH 多表示层、HLS 多 discontinuity 解析，映射到统一时钟 |
| `internal/scan` | 分段索引与 SHA-256 摘要绑定、截断检测 |
| `internal/transport` | pion/rtp 去重、重排、突发丢失静音填充（不压缩时间线） |
| `internal/audio` | 按采样时间重采样、声道归一、流式响度/峰值/静音统计 |
| `internal/packager` | go-mp4 再封装，目标码率 CBR 填充，实际码率计算 |
| `internal/report` | 量化指标 JSON 行 |
| `internal/fixture` | 种子确定的正常/损坏/边界样例（重复、乱序、突发丢失、带宽骤降、时间戳回退、截断） |
| `internal/run` | 流式执行编排、分段边界检查点续跑 |

关键语义：

- 时间定位一律基于**采样时间**而非包到达时间；带宽变化只影响读取节奏。
- 突发丢失按策略填充静音或停止，禁止静默压缩时间线。
- discontinuity 必须由 `EXT-X-PROGRAM-DATE-TIME` 给出显式重置映射，
  否则拒绝（无法无歧义映射）。
- 续跑状态（检查点）在摘要 mismatch、检查点截断、输出尾部不完整时
  一律丢弃并重新扫描。
- 同一输入与参数重复运行产生一致的分段顺序、统计值与清单。
- 日志与报告不含媒体载荷。
