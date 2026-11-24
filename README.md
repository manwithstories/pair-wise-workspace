# mediad — 多机位 CMAF/fMP4 分片核验与码率对齐库

纯本地、无外部素材与网络依赖的 Go 1.25.6 项目。样例全部由代码确定性生成
（种子固定），依赖仅 `github.com/abema/go-mp4 v1.7.4` 与
`github.com/pion/rtp v1.10.6`。不做转码。

## 快速验收

```bash
go mod tidy && go mod download && go mod verify
go build -o "$TMPDIR/mediad" ./cmd/mediad
go test -race ./...

"$TMPDIR/mediad" demo --case clean --seed 23 --workers 4 --queue 8 --out "$TMPDIR/clean"
# fragments=24 roundtrip=24/24 keyframe_hit=24/24 sha256=24/24
# representations=3 aligned=8                                  (exit 0)

"$TMPDIR/mediad" demo --case damaged --seed 23 --out "$TMPDIR/damaged"
# quarantined=2 roundtrip=22/22 sha256=24/24                    (exit 2)

"$TMPDIR/mediad" demo --case cancel --checkpoint "$TMPDIR/recovery"   # exit 3
"$TMPDIR/mediad" resume --checkpoint "$TMPDIR/recovery"
# resumed=1 duplicate_emit=0 result_hash_equal=true             (exit 0)
```

也可直接运行 `scripts/acceptance.sh` 一键复核上述全部断言。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 完整交付，清单与核验全部通过 |
| 2 | 存在已隔离的损坏片，但清单仍有效（哈希/往返核验通过） |
| 3 | 已取消并留下可续跑检查点 |
| 1 | 不可恢复的输入或配置错误（含 Input/Config 摘要不匹配） |

## 包结构（可复用库）

- `internal/core`：共享时钟、`Fault` 错误模型（首错偏移）、退出码、128 MiB 上限、整数有理数换算与溢出检查。
- `internal/mp4`：越界安全的盒扫描；`mfhd/tfdt/trun` 与同步标志解析；确定性编码与字节级 RoundTrip；`mdhd/hdlr/stsd/colr/mp4a/spkl` 元数据；并用 `go-mp4` 交叉校验。
- `internal/source`：输入发现/分类、确定性 InputDigest、有限 worker 池、背压通道、用 `pion/rtp` 解码 RTP 时间戳旁证。
- `internal/timeline`：摘要去重、序号缺口/乱序/重复记录、RTP 旁证判别 tfdt 回退（证据连续则修复；缺失/矛盾则开启显式新周期）、有理时基换算、DTS/PTS 溢出与单调检查、共同关键帧对齐（偏差超一帧即标记不可切换，不挪动边界）。
- `internal/metadata`：原样保留多声道扬声器布局、采样率与 nclx 色彩三元组；冲突时拆分音频组或标记视频表示层不兼容。
- `internal/manifest`：清单模式、确定性 `ResultHash`、临时文件 + rename 原子提交、SHA-256 与 RoundTrip 核验。
- `internal/pipeline`：编排、确定性归并、取消传播、绑定 Input/Config 摘要的检查点与续跑。
- `internal/fixture`：clean/damaged/cancel/元数据冲突及缺片、乱序、重复、tfdt 回退等代码夹具。
- 根包 `mediad`：对外门面（`Inspect` / `InspectWithCheckpoint` / `Resume` / `DryRun`）。

## 并发与恢复语义

任务与结果通道均有界（容量取 `--queue`），慢消费者经“结果通道打满 →
worker 阻塞 → 任务队列打满 → 生产者阻塞”形成背压。归并按
周期、表示时间、表示层、序号、摘要排序，`ResultHash` 不受 worker 数与调度影响。
取消沿 `context.Context` 传播到生产者、worker 与结果发送；只有完整记录会被
顺序提交并原子写入检查点。续跑先校验输入摘要与配置摘要，仅重做未提交项；
完成态再次 `resume` 幂等，不会重复写清单。
