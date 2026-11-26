# archivectl

音像档案馆多声道 PCM/WAV 扫描、时钟归一化、索引、流式修复与逐片段校验工具。纯本地终端程序，不依赖外部服务或样例文件；所有夹具均由 `internal/fixture` 用固定种子生成。

## 环境

- macOS / Darwin，arm64 或 amd64
- Go 1.25.6（`go.mod` 中 `go 1.25.6`）
- WAV 编解码：`github.com/go-audio/wav v1.1.0`

## 验收演示

```bash
go mod tidy
go build -o /tmp/archivectl ./cmd/archivectl
go test ./...
go run ./cmd/archivectl demo \
  --seed 41 \
  --frames 196608 \
  --channels 6 \
  --inject wrap,drift,vfr,bad-crc,truncate,missing-cue
```

演示会在 `demo-work/` 下生成正常 WAV、损坏 WAV、时钟记录、区段 CRC、基准修复 WAV、索引、修复结果和重跑结果。关键输出包括：

```text
roundtrip_rate=1.000
keyframe_hit_rate=0.990
damage_spans=4
recovered_frames=16384
sha256_segments=48
sha256_match=48/48
```

也可直接运行 `scripts/acceptance.sh`。

> 注意：Go 工具会把 `go run` 子进程的非零退出码包装为 `go run` 自身的退出码 1，并打印 `exit status 2`。archivectl 二进制本身的退出码仍严格遵循下列约定；演示输出中也有 `exit_code=2`。

## 退出码

| 退出码 | 含义 |
|---:|---|
| `0` | 扫描/修复/校验完成且无损坏记录 |
| `2` | 已流式恢复，索引中保留损坏范围记录 |
| `1` | 格式不支持、损坏缺口超限、校验失败、内存限制或输入在执行期间发生并发变化 |

## 分阶段命令

```bash
/tmp/archivectl scan \
  --wav demo-work/damaged.wav \
  --clock demo-work/clock-records.json \
  --checks demo-work/block-checks.json \
  --index-out demo-work/damaged-index.json

/tmp/archivectl repair \
  --index demo-work/damaged-index.json \
  --out demo-work/repaired.wav

/tmp/archivectl verify \
  --repaired demo-work/repaired.wav \
  --baseline demo-work/baseline-repaired.wav
```

索引通过同目录临时文件 + `rename` 原子写入。索引键包含 `archive-index/v1` 版本、WAV SHA-256、时钟记录 SHA-256，以及可选校验记录摘要；任一输入变化都会使索引失效。

## 关键约束

- 仅支持原始 PCM（format tag 1），位深 8/16/24/32，声道 1–8。
- 校验 RIFF 块长度、奇数块填充、`block align`、字节率和采样帧对齐。
- 32 位设备时钟按 `2^32` 回绕展开；锚点漂移用有理数 `ticks/frame` 建模。
- 每个时钟点均从同一原点做绝对帧映射，避免逐块舍入误差累积。
- 变长块的样本数来自块记录，不假定固定块长。
- 修复流块不超过 4096 帧；执行器硬上限为 65536 采样帧，连续缺口超过该上限即失败。
- 未知 RIFF 块只记录到扫描报告和索引，不读取/执行其内容。
- 提交输出前重新计算 WAV、时钟和校验记录摘要；若输入在执行期间变化，删除临时输出并返回竞争错误，不提交混合结果。
- SHA-256 按 4096 帧分片；196608 帧正好形成 48 个分片，并与生成器的修复后基准逐片比较。

## 目录

```text
cmd/archivectl/          命令适配（demo/scan/index/repair/verify）
internal/core/           有理数时基、统一错误模型、内存上限
internal/wavscan/        RIFF/WAVE fmt/data/cue 扫描与格式校验
internal/clock/          32 位回绕、VFR 范围、漂移估计
internal/index/          归一化锚点/损坏范围与原子索引
internal/repair/         有界流式补零与原子提交
internal/verify/         CRC 区段定位和 4096 帧 SHA-256 分片
internal/fixture/        确定性样例与注入生成器
wavscan/ clock/ index/ repair/
                         对应子系统的独立验收测试
```
