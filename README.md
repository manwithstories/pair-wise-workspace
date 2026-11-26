# Broadcast MediaPipe

纯本地、无外部服务依赖的广播节目素材处理流水线。它使用 Go 1.25.6 与
go-astiav/FFmpeg 完成 MP4/MOV 元数据解析、解封装和区间重封装，并在共享有理数时基上
完成切段、静音分析、两段式指纹去重、弱网传输恢复、寻址缓存、原子检查点和交付清单。

## 环境

```bash
brew install go ffmpeg pkgconf
go install golang.org/dl/go1.25.6@latest
go1.25.6 download
export PATH="$(go env GOPATH)/bin:$PATH"
```

本机验证版本为 FFmpeg 9.x。`go.mod` 保留规格要求的
`github.com/asticode/go-astiav v0.40.0` 版本钉点；由于 v0.40.0 的 CGO 绑定面向
FFmpeg 8，在 FFmpeg 9 头文件下会引用已删除的编码常量，因此通过显式
`replace ... => v0.43.0` 使用兼容 FFmpeg 9 的绑定。`go-astikit` 仍锁定 `v0.42.0`。

## 验收命令

```bash
go1.25.6 mod tidy
go1.25.6 build ./...
go1.25.6 test ./...
go1.25.6 run ./cmd/mediapipe demo \
  --work .tmp/demo \
  --seed 17 \
  --seconds 30 \
  --target-kbps 1200 \
  --fault-suite
```

也可以运行：

```bash
./scripts/acceptance.sh
```

演示会在工作目录内程序化生成 MP4、PCM 特征、缓存、检查点、分段 MP4 和
`manifest.json`，不引用任何外部素材。

## 终端 JSON 输出

正常素材行包含：

- `target_kbps=1200`
- `actual_kbps` 位于 `1140..1260`
- `deviation_pct <= 5`
- `decode_errors=0`
- `silence_regions=2`
- `duplicate_segments=1`
- `manifest_error_ms <= 42`

恢复行包含：

- `duplicate_packets`
- `reordered_packets`
- `recovered_packets`
- `cache_hit`
- `source_ok`
- `sha256_ok`

故障行包含 `case`、`error_code` 和数字 `exit_code`。`--fault-suite`
中所有“预期拒绝”都视为套件通过，命令最终返回 0。

## 退出码

`go run` 会把非零程序退出码包装为 1；验收退出码请直接运行构建产物：

```bash
go1.25.6 build -o .tmp/mediapipe ./cmd/mediapipe
./.tmp/mediapipe verify-fixture --case truncated_moov       --work .tmp/faults; echo $? # 2
./.tmp/mediapipe verify-fixture --case timestamp_rollback   --work .tmp/faults; echo $? # 2
./.tmp/mediapipe verify-fixture --case unrecoverable_loss   --work .tmp/faults; echo $? # 3
./.tmp/mediapipe verify-fixture --case digest_tampered      --work .tmp/faults; echo $? # 3
./.tmp/mediapipe verify-fixture --case checkpoint_error     --work .tmp/faults; echo $? # 4
./.tmp/mediapipe verify-fixture --case io_error             --work .tmp/faults; echo $? # 4
```

约定：

| 场景 | 退出码 |
| --- | ---: |
| 单文件损坏、截断 moov、时间戳回退、解封装/解码错误 | 2 |
| 不可恢复丢包、来源摘要不符、产物 SHA-256 不符 | 3 |
| I/O、原子写、检查点损坏/版本冲突 | 4 |

## 代码结构

```text
cmd/mediapipe/          参数适配、JSON 行输出、退出码映射
internal/media/         有理数时基、统一错误、路径收敛、原子写、资源限制
internal/source/        go-astiav 解封装、MP4 box/edit list、合成夹具生成
internal/transport/     固定种子弱网模拟、去重、重排、检查点续取
internal/cut/           目标时间切段、关键帧/GOP 前缀、一帧 A/V 偏差校验
internal/audio/         RMS、进入/退出迟滞、最短静音过滤、PCM16 转换
internal/fingerprint/   音频窗口 + 抽样帧初筛 + 全量摘要复核
internal/cache/         源摘要+编码配置寻址、命中再校验、容量淘汰
internal/checkpoint/    产物落盘并哈希后原子更新 JSON 日志
internal/encode/        码率预算、free-box 速率填充、astiav MP4 重封装
internal/manifest/      清单、统计口径、恢复字段、JSON Lines
internal/pipeline/      增量编排、30 秒演示指标、故障套件
```

## 可信与恢复约束

- 所有产物先写同目录临时文件，`fsync` 后原子 `rename`。
- 检查点只在分段文件最终落盘并完成 `stat`/SHA-256 后更新。
- 缓存键包含源文件 SHA-256 与编码配置摘要；每次命中仍重新计算来源和产物哈希。
- 路径参数经 `media.SafeJoin` 收敛，拒绝绝对路径、`..` 越界和符号链接逃逸。
- 输入大小、缓存条目/容量和并发上限由 `media.Limits` 约束。
- 时间戳回退返回流号与文件位置，不做静默钳制或修正。
- 固定种子保证弱网模拟和演示指标可重复。
