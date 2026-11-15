# relayd — 云游戏试玩串流转发节点离线验证工具

回环（127.0.0.1）专用的 WebRTC 转发节点与自检工具：一个入口
PeerConnection 收流，按需复制转发给多个出口 PeerConnection。信令、
ICE 候选过滤、RTP 抖动与丢包统计全部离线可复现。

## 构建

Go 1.22+，依赖已全部 vendor，无需网络：

    go build -mod=vendor ./cmd/relayd

## 子命令

    relayd selftest --peers 3 --packets 2000 --rate 500 --jitter-ms 8 --out report.json
    relayd signal --addr 127.0.0.1:0 --expect-reject 1
    relayd forward --listen 127.0.0.1:9000 --egress 3 --out report.json
    relayd report --file report.json

- `selftest`：确定性端到端自检。真实建立 1 条入口 + N 条出口
  PeerConnection（仅回环 host 候选），loadgen 以固定种子生成带抖动
  模型的合成 RTP；同一参数重复运行输出逐字节一致。
- `signal`：行式 JSON 信令服务器。`--expect-reject N` 运行内置探针，
  发送重复序号消息并校验 `rejected` 计数。
- `forward`：生产形态转发节点。启动信令服务器，等待 1 个 source 与
  N 个 sink 拨入（角色通过 hello 消息声明），按到达时间估计抖动，
  退出时落盘报告。
- `report`：回读报告 JSON 并做一致性校验。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 成功 |
| 2  | 信令失败 |
| 3  | ICE / 连接失败 |
| 4  | 统计不一致 |

## 报告字段

`candidate_pairs`（每出口腿 2 个，双向各一）、`forwarded_packets`、
`dropped_packets`（队列满丢最旧）、`jitter_ms`（RFC 3550 到达间隔
抖动的 mean/max/p99/samples）、`egress`。

## 布局

    cmd/relayd        子命令、参数解析与退出码
    internal/signal   行式 JSON 信令：协议、服务器、握手客户端
    internal/peer     仅回环 ICE 的 PeerConnection 构造与候选过滤
    internal/forward  多出口分发、SSRC/序号/时间戳重写、抖动估计
    internal/loadgen  可配置速率与抖动的合成 RTP 源（固定种子）
    internal/report   字段有序 JSON 报告与回读校验
