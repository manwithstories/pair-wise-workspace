# MediaRig

广播级 RTP/RTCP 包流质量检查、归一化、MP4 再封装与可恢复批次工具。纯本地
Go 1.25.6 程序，不调用外部媒体或在线服务。

## 快速验收

```sh
go mod download
go mod tidy
go build -o "$TMPDIR/mediarig" ./cmd/mediarig
go test ./...

"$TMPDIR/mediarig" demo --root "$TMPDIR/rig-clean" --case clean \
  --rate 48000 --layout stereo --lufs -23 --target-kbps 1536

"$TMPDIR/mediarig" demo --root "$TMPDIR/rig-damaged" --case damaged \
  --drop 5 --reorder 7 --wrap-seq --wrap-ts --color-conflict

"$TMPDIR/mediarig" demo --root "$TMPDIR/rig-boundary" \
  --case truncated --truncate-last

"$TMPDIR/mediarig" run --root "$TMPDIR/rig-recover" --fail-after audio
test $? -eq 3
"$TMPDIR/mediarig" resume --root "$TMPDIR/rig-recover"
"$TMPDIR/mediarig" audit --root "$TMPDIR/rig-recover"
```

也可运行 `scripts/acceptance.sh` 完成上述端到端检查。

## 支持的枚举

- RTP 负载：`L16`（PT 96）、约束/单 NAL H.264（PT 97）。
- 采样格式：RTP L16 big-endian、MP4 PCM signed 16-bit little-endian、内部 float DSP。
- 输入布局：`mono`、`stereo`、`5.1`、`7.1`，均混音为立体声。
- 采样率：8000、16000、24000、44100、48000 Hz。
- 色彩：BT.601、BT.709、BT.2020；默认优先级为 BT.709 → BT.2020 → BT.601。

5.1/7.1 声道顺序和混音系数在 `internal/audio/layout.go` 中显式定义：
中心声道为 `-3 dB`（`1/√2`），LFE 每声道系数为 `0.5`，环绕声为 `-3 dB`。

## 退出码

| 码 | 含义 |
|---:|---|
| 0 | 完成、恢复成功或 audit 成功 |
| 1 | 内部或本地 I/O 错误 |
| 2 | 输入损坏、色彩/丢包策略或路径策略拒绝 |
| 3 | 注入的可恢复中断 |

## 批次目录

```text
<input>/stream.mrr          类型字节 + uint32 BE 长度 + RTP/RTCP 原始包
input/source.json           夹具参数与源色彩元数据
wal/wal.log                 带 SHA-256 校验和的阶段 WAL
state/params.json           恢复时复核的参数
state/COMMITTED             原子提交标记
work/                       已校验阶段产物
output/master.mp4           同目录临时文件 + fsync + 原子改名
report/manifest.json        有理时间基准节目清单
report/stats.env            单行稳定 key=value 统计
```

限制：输入总量 512 MiB、单条长度前缀记录 1 MiB、并行批次 4 个。所有媒体
时长使用整数采样数或 `media.Rational`；float64 仅用于 DSP 系数、测量和最终
报告显示。
