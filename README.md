# MediaCheck 批量质量核验与再封装内核

纯本地、纯终端的 Go 1.25.6 服务。它从确定性夹具生成 H.264/H.265 Annex-B、AV1 OBU、关键帧索引、s16le PCM、RTP 包和慢消费者场景，经过有界队列、音频转换/响度控制、fMP4 封装、WAL 事务提交，再由清单与统计模块复核。

## 一键验收

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
go1.25.6 mod tidy
go1.25.6 build ./...
go1.25.6 test ./...
go1.25.6 run ./cmd/mediacheck verify \
  --out "$TMPDIR/media-demo" \
  --faults truncated,stale-index,rtp-wrap,slow-reader,crash-after-mux \
  --resume
```

也可以执行：

```bash
./scripts/acceptance.sh
```

正常样例会打印：

- `target_bps` / `actual_bps` / `deviation_pct`：容器总码率偏差约 `-0.3%`；
- 每个关键帧分片的逐段码率，偏差小于 `5%`；
- `audio_lufs=-23.00`、`true_peak_dbfs<=-1`；
- `manifest_skew_us=0`，满足 `<=1000`；
- `frame_count=60` 和完整 `sha256`。

损坏样例会打印 `error_stage`、`error_kind` 和 `error_offset`。恢复样例会打印
`wal_recovered_stages=ingest,keyindex,audio,mux`、WAL/文件/本次输出哈希以及
`hash_match=true rerun_hash_match=true`。

## 退出码

| 码 | 含义 |
|---:|---|
| `0` | 正常路径与所有预期故障路径均核验通过 |
| `2` | 素材、索引或时间线无效 |
| `3` | 资源、路径、队列或流水线错误 |
| `75` | 可恢复中断；配合 `--resume` 可从已提交 WAL 阶段恢复 |

演示命令中的故障是“预期损坏路径”：它们必须被正确分类，因此总体验收进程仍为 `0`；输出中的 `expected_exit_code=2` 和注入崩溃行的 `exit_code=75` 是对应真实错误的退出码契约。

## 模块

- `internal/media`：共享 90 kHz 时间基准、PTS/RTP 回绕判断、含阶段/偏移/PTS/可重试标记的错误模型。
- `internal/ingest`：H.264/H.265 Annex-B、AV1 OBU、s16le PCM、JSON 关键帧索引和 pion/rtp 校验。
- `internal/stream`：同时限制条目数与字节数的 FIFO，支持背压、取消唤醒和已处理位置。
- `internal/audio`：线性重采样、单/双声道转换、确定性 K-weighting LUFS、真峰值估算和峰值上限。
- `internal/mux`：基于 `github.com/abema/go-mp4 v1.7.4` 写 ftyp/moov/mvex/moof/traf/trun/mdat。
- `internal/txn`：JSONL WAL、临时产物、SHA-256/时长校验、原子 rename、崩溃恢复和安全路径。
- `internal/manifest`：视频、音频、索引三方时长/点数/偏移对齐。
- `internal/metrics`：整体与逐关键帧分片码率、`±5%` 容差和统计。
- `internal/fixture`：所有正常与损坏夹具均由 Go 代码生成。
- `internal/demo`：验收场景编排。
- `cmd/mediacheck`：参数入口和 32 MiB Go runtime soft memory limit。

## 安全与资源约束

- 所有产物路径通过 `txn.SafeJoin` 限制在 `--out` 指定根目录内；绝对路径和 `..` 穿越返回 `path_traversal`。
- 队列默认演示预算为 32 KiB；慢消费者测试使用 4 KiB 字节上限并观察高水位。
- CLI 默认调用 `debug.SetMemoryLimit(32 MiB)`，也可用 `--memory-mib` 调整。
- 视频只做解析和再封装，不做视频重编码；报告固定输出 `video_reencoded=false`。
- 输出时间字段和测试数据均为确定性生成，重复全新作业的 `sha256` 相同。
