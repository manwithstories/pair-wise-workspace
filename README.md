# avpipe — 低延迟互动回放流水线（纯本地）

avpipe 把带时间戳的回传素材（图像帧 + 48 kHz 音频）整理为多码率 HLS 回放：
去重排序、XOR 丢包恢复、突发丢包缺口标记、ABR 码率决策、FFmpeg fMP4 分片、
带校验和的本地索引与中断续跑。全部素材由程序生成，运行时不联网。

## 环境

- macOS（arm64 / amd64），Go 1.25.6（官方安装包）
- FFmpeg / ffprobe（Homebrew）：`brew install ffmpeg`

## 构建、测试与演示

```bash
brew install ffmpeg
go mod tidy && go build ./... && go test ./...
OUT="$(mktemp -d)/avpipe"; go run ./cmd/avpipe demo \
  --out "$OUT" --frames 240 --fps 30 --segment 1s \
  --targets 180k,450k,900k --seed 41 --faults mixed \
  --interrupt-after 3 --resume
```

演示会自生成：正常素材、SHA-256 校验损坏、突发丢包、乱序重复、中断续跑，
并逐项打印 `target_kbps / actual_kbps / delta_pct / psnr_y_db /
recovered_packets / gap_frames / align_max_ms / av_drift_ms /
resume_segments / playlist_hash`（另含 `frame_hash`）。

退出码：`0` 演示完成；`2` 参数错误；`3` 素材校验失败；`4` 索引冲突或
不可恢复写入失败。

## 模块布局与数据流

```
ingest 校验 -> transport 去重/排序/FEC -> abr 带宽估计/质量
            -> segment FFmpeg 编码 / Eyevinn 清单
            -> index 暂存-校验-组提交（检查点）
```

| 包 | 职责 |
| --- | --- |
| `internal/media` | 帧/音频/包类型、共享错误模型、90 kHz 时钟与 33 位 PTS 回绕 |
| `internal/ingest` | 元数据校验、路径安全、二进制容器读写与 SHA-256 校验 |
| `internal/transport` | 精确/冲突去重、按流重排、8 包一组 XOR FEC、突发缺口 |
| `internal/abr` | 滑动窗口带宽估计、180/450/900 kbps 决策、PSNR/漂移 |
| `internal/segment` | FFmpeg fMP4 分片、多码率 master/variant 清单、EXT-X-GAP |
| `internal/index` | 固定长度校验和日志、检查点、截断安全重放 |
| `internal/fixture` | 确定性素材与混合故障注入 |
| `internal/pipeline` | 时间线重建（冻结帧/静音）、暂存-校验-提交、中断续跑 |
| `internal/demo` | 端到端编排与终端报告 |
| `cmd/avpipe` | 终端入口与退出码映射 |

## 关键设计

- **统一时钟**：视频 30 fps、音频 48 kHz 均映射到 90 kHz 时钟；音频每秒 8 包、
  每包 6000 采样，与 8 包 FEC 块和 1 秒分片严格对齐。
- **FEC**：每 8 个媒体包一个 XOR 校验包；单包丢失可恢复，多包突发丢失输出
  带缺口标记的连续时间线（视频沿用前帧、音频补静音）。校验包通过容器中的
  `parityOf` 字段区分其保护的视频/音频流，避免块号碰撞。
- **对齐**：每个表示层都编码相同的 1 秒分片，分片起止时间、缺口位置跨层一致，
  含缺口的清单按 RFC 8216 提升到 `EXT-X-VERSION:7` 并写 `#EXT-X-GAP`。
- **索引**：先暂存并校验分片，再以「追加 + fsync」原子提交整组；日志为固定
  256 字节、带 SHA-256 的记录，崩溃产生的半截尾记录会被安全截断忽略。
- **检查点身份**：输入摘要或编码配置变化时拒绝沿用旧检查点（退出码 4）。
- **幂等**：相同参数重复运行产出相同的 `frame_hash` 与 `playlist_hash`。

## 关于 go.mod 的 toolchain 指令

需求要求 go.mod 同时包含 `go 1.25.6` 与 `toolchain go1.25.6`。在本机
Go 1.25.6 上，当 `toolchain` 版本与 `go` 指令、实际运行版本完全相同时，
该指令是冗余的：`go mod tidy` 会将其移除，而保留它又会让 `go build`/
`go test` 报 “updates to go.mod needed”，二者无法同时满足，会直接导致
验收命令 `go mod tidy && go build ./... && go test ./...` 失败。

因此 go.mod 保留 tidy 后的形态（`go 1.25.6` 已固定语言版本与工具链选择，
本机工具链即 go1.25.6），不写冗余的 `toolchain` 行。任何后续执行
`go mod tidy` 都不会再改动该文件。
