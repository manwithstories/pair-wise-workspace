# mediad — 多轨 RTP 音频统一时间轴流水线

远程制作场景：录音棚（studio）、外景采访（field）、远端配音（remote）三路
RTP 音频汇聚到统一有理数时间轴，完成重排去重、访问单元解析、切分、静音
检测、指纹去重、MP4 往返与检查点恢复。纯终端运行，无外部服务依赖。

## 构建与验收

```sh
go mod tidy && go mod download && go build ./... && go test ./...
go run ./cmd/mediad verify --seed 17 --fault-suite
```

依赖固定在 go.mod：`github.com/abema/go-mp4 v1.4.1`、
`github.com/pion/rtp v1.8.12`，Go 1.25.6。

## 模块

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 统一有理数时间基准（48k/44.1k/90k 精确换算） |
| `internal/fault` | 可序列化错误模型，携带阻塞依赖信息 |
| `internal/rtp` | 按 SSRC 重排、去重、冲突隔离、自适应等待窗、显式缺包 |
| `internal/codec` | Opus TOC / AAC 可变时长访问单元解析，时基回退检测 |
| `internal/timeline` | 多轨统一网格切分，显式间隙，不伪造音频 |
| `internal/silence` | RMS 静音区间检测（跳过编解码头字节） |
| `internal/fingerprint` | 分段 SHA-256，访问单元级跨轨去重并保留来源映射 |
| `internal/dag` | 依赖 DAG 调度：幂等键（输入哈希+参数）、重试、拓扑序首个阻塞节点 |
| `internal/checkpoint` | 本地检查点：原子写入、CRC 校验、损坏报错 |
| `internal/mp4` | MP4 封装/解析往返，逐字段定位首个差异，随机访问索引 |

## 验收输出

`verify --seed 17 --fault-suite` 逐段打印 `seg N <64 位十六进制 sha256>`，
并输出：

```
roundtrip=100.00%
keyframe_hit=100.00%
segment_hashes=12/12
retries=2
first_blocked=decode:input-4
```

损坏样例（突发丢包 + 冲突包 + 超窗迟到包）报告缺口，且未受损片段的
哈希与基线完全一致；检查点损坏被检测后重建，恢复运行幂等跳过全部
节点且片段哈希一致。

## 退出码

- `0`：全部断言通过
- `1`：断言失败
- `2`：输入损坏（正常路径出现缺口/冲突）
- `3`：重试耗尽且存在阻塞依赖（主流水线 DAG 被阻塞）

所有输入由程序按种子生成，检查点写入工作区内 `.mediad/`，不读取任何
真实外部素材。
