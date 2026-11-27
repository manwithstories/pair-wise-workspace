# mediacore — 跨节点媒体处理内核

面向区域直播分发中“跨节点媒体传输”的可复用内核：边缘节点收到的字幕、章节、
ID3、emsg、SEI 等跨轨定时元数据，与音频共享同一条统一时间线；音频被归一到
48 kHz 双声道并按 −23 LUFS 控制响度；处理过程有界、可背压、可检查点恢复，
且重复运行得到逐字节相同的片段哈希。

全部逻辑位于本地，无外部服务依赖。媒体解封装与编码通过 go-astiav 调用本机
FFmpeg 9；重采样、响度测量、时基换算、哈希与流水线为纯 Go 实现。

## 环境

- macOS（Darwin，Apple Silicon 或 Intel）
- Go 1.25.6
- FFmpeg 9 与 pkg-config

```sh
# Go 1.25.6（按架构选择）
#   https://go.dev/dl/go1.25.6.darwin-arm64.pkg
#   https://go.dev/dl/go1.25.6.darwin-amd64.pkg
sudo installer -pkg go1.25.6.darwin-arm64.pkg -target /

brew install ffmpeg pkgconf
```

`go.mod` 固定 `go 1.25.6`，`toolchain go1.25.6`。

## 快速验证

```sh
go mod tidy
go build ./...
go test ./...

# 正常路径：自生成夹具、12 个片段、慢消费者、64 MiB 队列上限
go run ./cmd/mediacore verify --self-generate --segments 12 \
  --slow-consumer 40ms --memory-limit 64MiB

# 损坏样例 + 崩溃恢复：截断记录被隔离，进程在 5 段后中断并恢复
go run ./cmd/mediacore verify --self-generate --fault truncate \
  --simulate-crash-after 5 --resume
```

正常输出包含：

```
roundtrip_rate=100.00%
keyframe_hit_rate=100.00%
segment_sha256_match=12/12
queue_peak_bytes=... queue_limit_bytes=67108864
```

恢复输出包含：

```
index_invalidated=1
recovered_segments=5
quarantined kind=subtitle track=2 t_us=2000000 reason="..."
```

注意：`go run` 在子进程以非零码退出时会返回 1。验收要求“退出码 2 表示损坏
样例已隔离并报告”，用构建后的二进制或 `make verify-fault` 可直接观察：

```sh
go build -o bin/mediacore ./cmd/mediacore
./bin/mediacore verify --self-generate --fault truncate ; echo "exit=$?"   # exit=2
```

## 处理模型

数据流：扫描器 → 索引缓存 → 有界流式执行器 → 分片输出与哈希报告。

| 模块 | 职责 |
| --- | --- |
| `internal/clock` | 有理数时基、统一时间线、统一错误（携带 track 与时间位置） |
| `internal/scan` | 解封装扫描：轨道、关键帧、字幕、timed ID3、emsg、SEI、章节 |
| `internal/index` | 关键帧与元数据索引，绑定输入 SHA-256，摘要变化时原子失效并重建 |
| `internal/meta` | 字幕/章节/ID3/emsg/SEI 往返编解码，越界与截断返回定位错误 |
| `internal/audio` | 48 kHz 双声道归一、Kaiser 窗 sinc 重采样、BS.1770 响度 |
| `internal/hash` | 逐片段 SHA-256，长度前缀防止拼接碰撞 |
| `internal/stream` | 字节预算队列、背压、取消、检查点恢复 |
| `internal/decode` | astiav 音频解码，按片段窗口重置解码器 |
| `internal/fixture` | 纯 Go 自生成媒体样例（不可引用外部文件） |
| `cmd/mediacore` | 终端适配器 |

### 时间线

所有轨道共享一个容器原点（各轨最早起点），字幕与元数据因此保留相对音频的
真实偏移。`clock.Convert` 用 `math/big` 精确重缩放，拒绝负时长、负时间戳与
int64 溢出，并在错误中报告轨道与时间位置。

### 有界与背压

队列按字节预算（默认 64 MiB）预留，`Prepare` 在生产者侧逐片段解码，慢消费
者会让生产者在 `sync.Cond` 上等待；取消时唤醒阻塞者、清零预留字节，并保留
最后一个已确认检查点。

### 确定性

每个片段使用独立的解码器与重采样器，输入窗口只取决于片段起止时间，因此
“完整运行”与“崩溃后恢复”对同一片段计算出的 SHA-256 完全一致。

### 可复现的演示

`./scripts/demo.sh` 一次运行两条验收命令，并检查关键指标。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 完成，无隔离记录 |
| 2 | 损坏样例已隔离并报告（扫描结果仍可用） |
| 1 | 内部错误或输入/输出错误 |

## 设计取舍

- **重采样与响度用纯 Go**：go-astiav v0.42.0 未暴露 libswresample 的
  `swr_alloc_set_opts2` / `swr_init`，无法配置转换参数；纯 Go 实现同时让
  每个阶段可直接单测，并与 FFmpeg `ebur128` 交叉校验（误差 < 0.5 LU）。
- **每片段重置解码器**：AAC 跨包重叠没有公开 flush 接口；重开容器解码是
  唯一与历史无关的复位方式，代价是片段级重复解码，换取可验证的确定性。
- **MP4 与 MPEG-TS 分工**：MP4 承载 A/V、字幕和章节；MPEG-TS 承载
  timed-ID3/emsg 数据轨。Matroska 不支持数据轨，生成器会明确拒绝。
