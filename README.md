# PairFlow MediaSync

纯本地、可复用的多轨媒体流水线库。它生成确定性的图像帧、48 kHz PCM 音轨和 RTP 样例，完成清单校验、时间戳回绕/漂移估计、抖动缓冲、XOR FEC、音画对齐、内容寻址缓存、检查点续跑与验收证明。

## 验收命令

在项目根目录执行：

```bash
go1.25.6 mod tidy
go1.25.6 mod download
go1.25.6 build ./...
go1.25.6 test ./...

proof="$TMPDIR/pairflow-proof-$$"
go1.25.6 run ./cmd/mediasync prove \
  --work "$proof" \
  --seed 417 \
  --tracks 4 \
  --frames-per-track 1200 \
  --payload-bytes 4096 \
  --drift-ppm 180 \
  --rollover 4294967000 \
  --vfr-seed 53 \
  --drop-seqs 811,812 \
  --damage-track cam-03 \
  --damage-frame 311 \
  --corrupt-byte 29 \
  --crash-after-records 720

cmp "$proof/cold/output.jsonl" "$proof/resumed/output.jsonl"
shasum -a 256 "$proof/cold/output.jsonl" "$proof/resumed/output.jsonl"
```

也可以运行 `./scripts/acceptance.sh`。

## 包结构

- `internal/model`：帧、轨道、统一时间区间和错误记录。
- `internal/ingest`：清单/来源/SHA-256 校验，以及单字节损坏的 XOR 定位子。
- `internal/clock`：32 位 RTP 时间戳回绕、设备重启识别和有界漂移估计。
- `internal/transport`：`github.com/pion/rtp v1.8.11` 编解码、截止时间抖动缓冲、重复/乱序/迟到/冲突分类和 XOR FEC。
- `internal/syncer`：可变帧率视频与 48 kHz PCM 音频的统一纳秒时基对齐，输出最大偏差和缺口区间。
- `internal/store`：覆盖来源指纹、输入摘要、规范化元数据和算法版本的 CAS；原子检查点与最近有效阶段恢复。
- `internal/pipeline`：确定性样例生成、增量编排、崩溃注入、续跑和 `prove` 报告。
- `cmd/mediasync`：唯一终端入口。

根目录的 `clock_test.go`、`jitter_test.go`、`syncer_test.go`、`pipeline_test.go` 是对外契约测试；各内部包另有同目录单元测试。

## 退出码

- `0`：不变量通过，预期损坏已精确定位和分类。
- `2`：不可恢复的媒体或来源错误。
- `64`：命令参数错误。
- `70`：内部错误。

## 设计约束

- 所有样例均由固定种子在本地生成，不读取外部媒体文件。
- 运行验收不访问网络或外部服务。
- 扩展时间戳保持单调；设备重启形成新的时间纪元。
- VFR 帧按各自 PTS/时长处理，不使用名义帧率补时间。
- 同序号但内容不同的 RTP 包返回 `sequence_conflict`，不会静默覆盖。
- 超出单包 XOR 恢复能力的连续丢包形成显式缺口，并传播到同步报告。
- 检查点只在输出 `write + fsync` 后原子提交；主检查点损坏时回退到最近有效阶段。
- 缓存命中会重新核对来源指纹、输入摘要和缓存块摘要，不匹配即拒绝复用。
