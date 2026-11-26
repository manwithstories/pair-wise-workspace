# avpipe

竞技手游双人复盘房间的 MPEG-TS → LL-HLS 切片、多码率对齐与检查点恢复验收工程。
所有素材均由 Go 代码用固定种子确定性生成，不读取外部素材或网络服务。

## 快速验收

```bash
go mod tidy
go build ./...
go test ./...
go build -o /tmp/avpipe ./cmd/avpipe

/tmp/avpipe demo --seed 23 --case clean \
  --bitrates 600000,1200000 --segments 12 --out /tmp/avpipe-clean
/tmp/avpipe verify --out /tmp/avpipe-clean

/tmp/avpipe demo --seed 23 --case damaged \
  --bitrates 600000,1200000 --segments 12 --out /tmp/avpipe-damaged

/tmp/avpipe demo --seed 23 --case truncated \
  --bitrates 600000,1200000 --segments 12 --out /tmp/avpipe-truncated

/tmp/avpipe demo --seed 23 --case clean \
  --bitrates 600000,1200000 --segments 12 \
  --interrupt-after 8 --out /tmp/avpipe-resume
# 该命令按契约返回 75

/tmp/avpipe resume --out /tmp/avpipe-resume
/tmp/avpipe verify --out /tmp/avpipe-resume
```

也可以运行 `./scripts/acceptance.sh` 自动执行并断言全部关键指标。

## 关键输出

干净流：

```text
roundtrip=100.00%
packets=1920
keyframe_hit=24/24
aligned_slots=12/12
segment_sha256=24/24
```

损坏流：

```text
cc_gap=1
psi_crc_reject=1
keyframe_hit=23/24
gap_slots=1
```

截断流报告 `incomplete_tail=1`；不足 188 字节的尾部只计入不完整输入，不会被补造成完整 TS 包或片段。所有已提交片段文件大小均为 188 的整数倍。

中断续跑：

```text
checkpoint_reused=8
rebuilt=4
idempotent=1
```

## 包结构

- `internal/media`：PCR/PTS 33 位回绕展开、时钟历元、统一错误与退出码。
- `internal/ts`：188 字节成帧、首失步偏移、截断尾统计、CC 跟踪、PCR 适配字段、PAT/PMT 重组、MPEG-2 CRC 与确定性构造器。
- `internal/index`：AVC IDR NAL 索引、PES PTS/音频时间戳、CC 影响区间和缺 IDR 槽位。
- `internal/align`：至少两种码率的共同槽位/时间刻度对齐；单边缺边界时保留序号并生成 GAP。
- `internal/hls`：基于 `github.com/Eyevinn/hls-m3u8 v0.6.5` 生成/解析主清单、媒体清单、`EXT-X-GAP`、`EXT-X-PART` 与 `EXT-X-SERVER-CONTROL`。
- `internal/checkpoint`：分块检查点、SHA-256、临时文件哈希后原子改名、JSON 索引、append-only journal、重启校验和孤儿清理。
- `internal/pipeline`：确定性素材、分析、切片、清单、溯源和恢复编排。
- `internal/fixture`：干净、PCR 回绕、CC 跳变、坏 CRC PSI、缺 IDR、截断和组合损坏夹具。
- `cmd/avpipe`：`demo`、`verify`、`resume` 终端入口。

## 恢复与溯源

输出目录包含：

- `sources/`：确定性生成的两种码率 TS 源。
- `segments/<rendition>/`：完整片段及 LL partial 文件。
- `chunks/chunk-XXXX.json`：每槽位不可变检查点。
- `journal.jsonl`：追加式提交日志。
- `checkpoint-index.json`：原子替换的检查点索引。
- `master.m3u8`、`<rendition>.m3u8`：HLS 清单。
- `report.json`、`provenance.json`、`recovery.json`：指标、源/配置哈希、时钟历元、输入偏移、片段哈希、解析器版本、PSI 拒绝、缺帧和 GAP 记录。

片段先写入同目录临时文件并在流式复制期间计算 SHA-256；`fsync`、关闭并原子 rename 后，才提交 chunk、journal 和索引。重启时会校验索引与 journal、重新计算所有已提交文件哈希，并删除未被索引引用的临时文件或孤儿片段。

## 退出码

- `0`：通过。
- `2`：参数、配置或不可信路径错误。
- `3`：不可恢复媒体或检查点错误，例如没有可信 PCR 锚点、已提交片段哈希不符。
- `75`：已按 `--interrupt-after` 完成并落盘指定检查点后的主动中断。
