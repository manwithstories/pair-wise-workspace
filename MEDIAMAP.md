# mediamap

多轨 HLS/DASH 素材的统一时间轴定位工具：解析清单与 MP4 分片、校准时间戳、
对齐多码率切点、恢复 RTP 丢包，并输出可核对的 JSON 清单与诊断摘要。
纯终端运行，不访问网络，不调用系统解码器；样例数据全部由代码生成。

```console
go mod download && go build ./... && go test ./...
go run ./cmd/mediamap demo --tracks 4 --ladder 3 --drop-burst 12 \
    --truncate-every 7 --seed 41 --json
go run ./cmd/mediamap verify --input generated/case.json
```

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 无故障 |
| 2 | 存在已标记的可恢复损坏（输出带不完整标记的清单） |
| 1 | 参数错误，或存在不可恢复的结构错误（该轨道终止，诊断保留） |

## 模块

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 90 kHz 统一时基；有理数换算；33/32 位回绕展开 |
| `internal/fault` | 带原始字节偏移、轨道、阶段、可恢复标记的错误模型 |
| `internal/manifest` | HLS master/media 与 DASH SegmentTemplate/Timeline 解析；缺索引时按时长推导边界并标记 `derived` |
| `internal/segment` | 逐盒解析 MP4 分片，保留源偏移；截断/越界可恢复，非法长度为致命错误；绝不越过输入缓冲区 |
| `internal/align` | 以清单声明的规范切点为参照对齐各码率层；受限关键帧回退；缺口显式输出，绝不静默延长邻段 |
| `internal/rtp` | 有界窗口重排去重；区分恢复/不可恢复/迟到；重试包绝不重复计数 |
| `internal/pipeline` | 以轨道为隔离单元编排全流程；输出指标、清单哈希与验证 |
| `internal/testkit` | 确定性生成正常/截断/坏长度/乱序/重复/突发丢失/不完整索引样例 |
| `cmd/mediamap` | `demo` / `verify` 子命令 |

## 设计要点

**时间戳归一。** 每个表示层的 tfdt 采集时钟先经 33 位回绕展开成单调序列，
再减去本层锚点、按 `EXT-X-START` 声明的呈现起点落到统一 90 kHz 时轴。
全部换算走 `clock.Rational`（约分后的 int64 分数），只在最终落 tick 时
四舍五入一次，长素材不累积漂移。

**呈现起点靠声明，不靠各自归零。** 音频轨相对视频轨的 3–18 ms 偏移由
`EXT-X-START:TIME-OFFSET` 声明并镜像在 tfdt 中；流水线重建该偏移作为
`av_sync_ms`，而不是把每路流各自归零后报 0。

**切点缺失显式化。** 规范切点取自清单声明的边界（索引不全时按
TARGETDURATION 推导并标记），即使所有码率层都缺同一段媒体，该区间也会
以 `gaps` 形式输出，而不会从对齐结果里消失。

**资源硬上限。** 单分片 64 MiB、单分片 4096 盒、盒嵌套 8 层、RTP 重排窗口
512 包；超限即报错，不会无界增长。
