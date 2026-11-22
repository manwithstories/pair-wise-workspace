# mediapack

面向远程制作团队的 Matroska/WebM 多轨素材流式处理流水线：把演播室、外景与
远端解说的交错音视频/字幕块解析为可校验的分片清单，让不同码率表示层共享一致
的时间边界，并支持慢速磁盘、消费者暂停与进程中断后的安全续跑。

纯本地、无网络、无转码。样例数据全部由代码确定性生成，不引用任何外部文件。

## 技术栈

- Go 1.23（`go.mod` 固定 `go 1.23`）
- `github.com/pion/rtp v1.8.11`：复用 RTP 序号（uint16）/时间戳（uint32）
  的模块化回绕边界模型
- `github.com/abema/go-mp4`：复用容器时间基准（`Mdhd.Timescale`）与媒体结构类型
- 标准库 `io`、`context`、`crypto/sha256`、`testing`

> **依赖版本偏差说明**：任务书要求 `go-mp4 v1.28.0`，但该上游仓库实际只发布到
> `v1.7.4`（`v1.28.0` 标签不存在，`go list -m -versions` 可证）。因此采用可用的
> 最新版 `v1.7.4`；`pion/rtp v1.8.11` 按要求精确固定。

## 架构与数据流

```
EBML 解析器（带原始字节偏移的块事件）
        │  未知长度 Segment/Cluster、有限元素、多轨交错块
        ▼
共享时钟 clock（纳秒基准、回绕解包、容差、统一错误类型）
        ▼
时间线 timeline（水位推进；越过水位才提交；迟到事件确定性回溯）
        ▼
有界流 stream（字节预算队列；满则阻塞回传解析器；取消不丢块）
        ▼
清单 manifest（公共时间刻度对齐 ≥2 表示层；缺轨/空分片/边界结构化错误）
        ▼
检查点 checkpoint（原子写入输入指纹、解析偏移、水位、输出摘要）
```

### 目录

| 包 | 职责 |
| --- | --- |
| `internal/ebml` | 流式 EBML/VINT 解析、未知长度作用域、块解码、截断精确定位 |
| `internal/clock` | 纳秒时间基准、MP4 timescale 桥接、RTP 风格回绕、统一 `MediaError` |
| `internal/timeline` | 水位状态机、迟到/重复事件、稳定排序、检查点约束的回放 |
| `internal/stream` | 字节有界阻塞队列、背压、慢消费者取消 |
| `internal/manifest` | 多表示层网格对齐、确定性 JSON、SHA-256 校验和 |
| `internal/checkpoint` | 临时文件 + fsync + rename 原子续跑状态、源/输出校验 |
| `internal/fixture` | 六类自生成夹具（正常/错序/未知长度截断/回绕/迟到/慢消费者） |
| `internal/pipeline` | 串接全部阶段的可恢复运行 |
| `cmd/mediapack` | `demo` / `verify` / `recover` 终端入口 |

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...            # 加 -race 可验证并发安全
```

## 验收演示

```sh
go run ./cmd/mediapack demo --generate --out /tmp/mediapack-demo \
    --queue-bytes 1048576 --watermark-ms 500
go run ./cmd/mediapack verify  --dir /tmp/mediapack-demo
go run ./cmd/mediapack recover --dir /tmp/mediapack-demo --interrupt-after 3
```

输出指标：`segments`（≥20）、`representations`（≥2）、`peak_buffer_bytes`
（≤队列预算）、`corrupt_offset_error_bytes`（≤1）、`resume_ms`（≤2000）、
`manifest_sha256`（64 位十六进制）。重复 `verify` 校验和恒定；`recover`
续跑产出的校验和与一次干净运行完全相同。

### 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 参数或运行错误 |
| 2 | 输入损坏，已给出字节偏移与轨道/类别 |
| 3 | 检查点或校验和不匹配，已定位字段 |

## 安全约束

- 限制单元素声明长度与总内存，拒绝超大声明长度
- 拒绝整数溢出（时间换算、偏移计算）与父子路径越界
- 截断错误锚定到物理流结束位置（首个不可读字节），定位误差 ≤1
- 队列满时阻塞而非丢块；失败后检查点保留最后提交边界
- 仅处理本地自生成或用户提供的 Matroska/WebM，不执行转码、不访问网络
