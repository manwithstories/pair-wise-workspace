# mediaproc

纯本地的 Go 1.25.6 媒体处理演示：自研流式 EBML/Matroska/WebM 解析、轨道解交错、
关键帧切段、Opus 静音检测、时间戳/轨道归一化指纹去重、有界队列、哈希/MP4/RTP
校验适配。样例 Matroska 数据由 `internal/sample` 用确定性种子生成，不读取外部媒体文件。

## 验收命令

```bash
go version
go mod download
go build ./...
go test ./...

go run ./cmd/mediaproc demo \
  --seed 41 \
  --out "$TMPDIR/av-pass" \
  --max-memory 8388608 \
  --queue 4 \
  --slow-consumer 20ms
# roundtrip=100.00% keyframe_hit=100.00% segments=6 hash_ok=6/6 \
# silence=2 dedup=1 peak_queue=4 peak_memory<=8388608
# Peak bytes are recorded as peak_memory in out/report.json.

go run ./cmd/mediaproc demo \
  --seed 41 \
  --out "$TMPDIR/av-recover" \
  --fault truncated-block \
  --recover \
  --max-memory 8388608 \
  --queue 4
# recoveries=1 first_error_offset=1517 segments=5 hash_ok=5/5
```

偏移由确定性夹具固定；测试使用 `sample.Fixture.FaultOffset` 核对，而不是依赖工作目录。
也可以直接运行：

```bash
./scripts/acceptance.sh
```

## 故障样例

| 场景 | 入口/测试 | 核对内容 |
| --- | --- | --- |
| 截断块 | `--fault truncated-block --recover` | 原始 SimpleBlock 偏移、下一已验证 Cluster 边界、5/5 哈希 |
| 非法 VINT | `--fault invalid-vint --recover` | 非法描述符偏移、重同步边界、恢复后段数 |
| 缺失 Cluster 结束标记 | `--fault missing-cluster-end` | 未知长度 Cluster 在 EOF 正确结束 |
| 错误 lacing | `internal/demux/lacing_test.go` | Xiph/Fixed 截断和不均分错误 |
| 满队列取消 | `--fault queue-cancel` 与 `internal/pipeline/runner_test.go` | 被阻塞元素偏移、未发布项清理、内存上限 |

重复运行会生成相同的 `input.webm`、JSON record、MP4 侧车和 `manifest.sha256`；
`internal/app/demo_test.go` 会比较两个输出目录中全部相对路径的 SHA-256。

## 数据流

```text
sample Matroska/WebM
  -> ebml.Reader（VINT/元素头/未知长度/有界重同步）
  -> demux.Parser（Info/Tracks、Cluster/SimpleBlock/BlockGroup/lacing、纳秒时间）
  -> segment.Cutter（目标时长吸附最近关键帧）
  -> audio.Detector（pion/opus 解码、20ms RMS 窗、双阈值迟滞）
  -> fingerprint.Store（去时间戳/轨道号，SHA-256 后再比较完整载荷）
  -> verify（段哈希、JSON 往返、Abema go-mp4 Probe 分片 MP4 侧车）
  -> pipeline.Runner（容量、字节内存预算、背压、取消清理）
  -> out/segments/*.json|*.mp4 + manifest.sha256 + report.json
```

`internal/rtp` 使用 `github.com/pion/rtp` 生成 RTP 包，并把统一纳秒时间换算到
RTP 时钟（Opus 为 48 kHz）。不支持的音频编码不会被解码或删除：原始块保留在片段中，
同时记录带轨道号和偏移的 `TrackError`。

## 内存与损坏安全

- EBML metadata/block 读取均有字节上限，不把无限输入文件整体读入内存。
- 队列同时受 `--queue` 数量和 `--max-memory` 字节预算约束。
- 慢消费者只增加排队等待，不会绕过内存记账。
- 损坏时不发布未验证片段；只有通过 Timecode 与结构块校验的 Cluster 才能作为恢复边界。
- 所有结构化错误位于 `internal/model.Error`，携带原始偏移、阶段、可恢复标志和恢复边界。

## 退出码

- `0`：处理完成，或可恢复损坏已按策略恢复。
- `1`：不可恢复输入错误。
- `2`：哈希、清单或 MP4 等验证不变量失败。

## 模块固定版本

`go.mod` 固定：

```mod
go 1.25.6
toolchain go1.25.6
```

仓库同时提交单模块 `go.work`，使本机 Go 1.25.6 在工作区模式下接受这两个固定指令。
依赖版本为 `github.com/abema/go-mp4 v1.7.4`、`github.com/pion/rtp v1.10.6`、
`github.com/pion/opus v0.1.0`。
