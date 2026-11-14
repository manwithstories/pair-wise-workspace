# relayctl

本地电竞回看链路探针与 RTP 中继验证工具。所有监听、候选、媒体和下行 UDP
都限定在回环地址；样例媒体由固定种子实时生成，不依赖外部文件或公网服务。

## 数据流

```text
fixture.Generate
  -> sender TrackLocalStaticRTP
  -> 回环 WebRTC (ICE + DTLS + SRTP)
  -> relay 接收 Track
  -> 序号重排 / 去重 / 迟到丢弃
  -> 改写 SSRC 与起始序号
  -> 回环 UDP sink
  -> JSON report -> verify
```

信令是自定义的换行分隔 JSON，仅承载 `hello/offer/answer/candidate/bye` 及
心跳/就绪/错误控制消息；媒体从不经过 TCP 信令。

## 快速开始

```bash
go build ./...
go test -race ./...

go run ./cmd/relayctl probe \
  --bind 127.0.0.1:0 --want-pairs 2 --json out/pairs.json

go run ./cmd/relayctl demo \
  --packets 3000 --duration 6s --seed 42 \
  --impair loss=0.01,dup=0.01,reorder=0.02 \
  --sink 127.0.0.1:0 --json out/report.json

go run ./cmd/relayctl verify \
  --report out/report.json \
  --min-forward-ratio 0.98 --max-jitter-p95-ms 30
```

一键执行 demo 与 verify：

```bash
./scripts/run_demo.sh
```

`probe` 默认使用两个回环 UDP mux 收集两个真实本地候选；对端相同，因此
候选对清单为 `2 x 2 = 4`，ICE 仅记录实际选中的一对。可用
`--inject-banned` 验证非法候选被整条拒收且策略退出码为 4。

## 子命令

- `probe`：信令配对、SDP 交换、trickle 候选、ICE 连通和候选对统计。
- `demo`：生成受损 RTP 序列，经 WebRTC、抖动缓冲和 UDP 转发后落盘报告。
- `verify`：读取报告，检查 `forwarded/emitted` 与 jitter p95 阈值。
- `sink`：独立回环 UDP sink；可配合 `demo --external-sink --sink ADDR` 使用。

`demo --sink 127.0.0.1:0` 会启动内置 sink 并报告实际端口。若要验证
“sink 未启动”，先运行 `sink` 获取端口，再关闭 sink 后用
`demo --external-sink --sink 127.0.0.1:PORT` 重跑。

## 指标口径

- `emitted`：夹具计划生成包数（包含模拟丢失的包）。
- `received`：首次到达的唯一序号数，不含重复副本。
- `unique`：进入抖动窗口并按序转发的唯一包数。
- `forwarded`：成功写入下游 UDP 的包数，正常结束时应等于 `unique`。
- `duplicates`：重复序号的额外副本数。
- `reordered`：首次到达时序号低于已见最高序号的包数。
- `late_dropped`：已越过发送位置或落在 64 包窗口外的唯一包数。
- `loss_ratio`：`1 - unique/emitted`。
- `forward_ratio`：`forwarded/emitted`。
- `forward_bytes`：成功写入下游 UDP 的完整 RTP 字节数。
- `jitter_ms.p50/p95/max`：RFC 3550 平滑抖动估计的采样分位数。

固定种子下包计数可复现；时间相关抖动允许小幅浮动。

## 退出码

| 码 | 含义 |
|---:|---|
| 0 | 成功且指标达标 |
| 1 | 参数或运行期错误 |
| 2 | 输入或报告非法 |
| 3 | 指标未达阈值 |
| 4 | 回环策略被违反 |

## 异常链路与提示

| 场景 | `error_code` | 典型提示 |
|---|---|---|
| 端口占用 | `port_in_use` | 更换 `--bind` 或停止旧监听 |
| 信令超时 | `signal_timeout` | 检查监听地址与双方启动顺序 |
| ICE 失败 | `ice_failed` / `ice_timeout` | 检查候选交换与回环过滤 |
| sink 未启动 | `sink_unavailable` | 启动 `relayctl sink` 或使用端口 0 |
| SDP 畸形 | `sdp_or_candidate_invalid` | 检查 SDP/候选字段 |
| 候选非法 | `candidate_banned` / `loopback_policy_violation` | 仅允许 `127.0.0.1`、`::1` |

## 代码结构

```text
cmd/relayctl/          唯一程序入口与 probe/demo/verify/sink 编排
internal/signal/       NDJSON 协议、回环信令服务器、断线重连客户端
internal/peer/         PeerConnection、SettingEngine、候选策略与选中对
internal/relay/        抖动缓冲、RTP 改写转发、统计与报告
internal/fixture/      固定种子受损包序列生成器
scripts/run_demo.sh    一键 demo + verify
```

本地回环测试关闭了 SRTP 重放保护，使重复 RTP 序号能到达抖动缓冲并由业务层
计数；该设置只用于本工具的回环验证，不应作为生产安全配置。
