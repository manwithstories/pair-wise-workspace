# review — 多机位复核材料流水线

现场演出后期：对齐多机位时间轴、合成字幕轨、离屏导出可追溯复核帧，
支持中断恢复与确定性校验。纯终端形态，无外部服务依赖。

## 构建与测试

```sh
brew install go@1.23 && export PATH="$(brew --prefix go@1.23)/bin:$PATH"
go mod tidy
go test ./...
```

## 演示（生成样例 → 注入损坏 → 中断 → 恢复）

```sh
go run ./cmd/review demo --work ./demo-work --cams 4 --fps 30000/1001 \
  --block-frames 120 --inject truncated-srt,damaged-mp4,incomplete-index \
  --interrupt-after-block 3
go run ./cmd/review resume --work ./demo-work --json
```

`demo` 重新生成全部自有样例（SRT/WebVTT、设备时间码 CSV、RTP 时间戳流、
go-mp4 写出的 MP4），按 `--inject` 注入尾部截断、损坏与不完整索引，
处理 3 个块后中断并保留可恢复状态。`resume` 校验检查点后续跑并输出
JSON 摘要。连续两次 `resume` 的最终 `sha256` 相同；不中断的完整运行
与"中断+恢复"的最终哈希也相同。

## 退出码

- `0` 完成且校验通过（demo 被 `--interrupt-after-block` 中断也算正常）
- `2` 输入存在已定位错误（重叠/非法/截断记录、时间码无法换算、RTP
  重复/倒退/缺失、MP4 截断/索引不完整）但可继续
- `3` 检查点或输出校验失败

## 架构

| 模块 | 职责 |
| --- | --- |
| `internal/clock` | 分数帧率（24/25/30/30000/1001）、SMPTE 丢帧/非丢帧、日界回绕；RTP 32 位时间戳展开与 MP4 解码时间对齐 |
| `internal/subtitle` | SRT/WebVTT 解析，记录字节偏移/时间范围/轨道来源；重叠、非法时间、截断行隔离为错误记录 |
| `internal/timeline` | 时间码清单解析（跨日回绕）与多轨分块合成，块边界保留来源映射 |
| `internal/media` | go-mp4 检查样本表，定位尾部截断与不完整索引到字节，不因单个损坏片段中止 |
| `internal/batch` | 分块检查点：临时文件 + 原子替换；恢复时校验输入指纹、配置摘要与已完成块校验和，从最近安全块重算 |
| `internal/render` | 离屏确定性复核帧（帧序号/媒体时刻/来源偏移/SHA-256），同输入必同哈希 |
| `internal/report` | JSON 摘要与退出码 |
| `internal/fixture` | 本地样例生成器（不引用外部素材） |
| `internal/run` | 流水线编排：并发多机位（在途块上限 4）、信号中断后完成当前检查点 |
| `cmd/review` | 终端入口（`demo` / `resume`） |

## 约束

- 单批内存远低于 512 MiB：逐块流式处理，在途块与待写块受限
- 输出写入 `work/out`，不覆盖 `work/inputs`
- 所有路径限定在工作目录内，拒绝符号链接逃逸（`internal/safepath`）
- 支持 macOS arm64 与 amd64
