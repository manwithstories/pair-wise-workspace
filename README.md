# rtspaudit

面向电竞赛事多机位归档与争议复核的离线 RTSP/RTP 审计工具。所有夹具、损伤和模拟服务都由代码在
`127.0.0.1` 上生成，不读取外部样本，也不连接任何外部地址。

## 快速演示

```sh
go run ./cmd/rtspaudit pull \
  --transport tcp --fixture auto --frames 120 \
  --loss 0.02 --reorder 0.01 --dup 0.01 --seed 7 \
  --out out/tcp.json

go run ./cmd/rtspaudit compare \
  --fixture auto --frames 120 --seed 7 --out out/cmp.json
```

`pull` 输出单传输统计；`compare` 对同一份夹具分别跑 TCP interleaved 与 UDP，输出
`tcp_frames`、`udp_frames`、`delta` 和 `verdict`。需要精确观察进程退出码时，请先构建二进制：

```sh
go build -o bin/rtspaudit ./cmd/rtspaudit
bin/rtspaudit compare --frames 120 --seed 7 --out out/cmp.json
```

`go run` 在子进程非零退出时自身只返回 1，因此不能用于核对 2/3/4 退出码。

## 审计内容

- 显式 RTSP 状态表覆盖 OPTIONS、DESCRIBE、SETUP、PLAY、PAUSE、TEARDOWN、会话超时、
  传输断开和 UDP 静默超时。非法事件不改变状态，只增加 `illegal_transitions`。
- TCP 解析 `$` interleaved 通道，按 Content-Length 切分 RTSP 响应正文，并缓存跨读半包。
- UDP 绑定连续的本地 RTP/RTCP 端口，校验 SETUP 回显参数；静默超时可重发 PLAY，
  超过 `--max-reconnects` 后进入失败态。
- RTP 处理 16 位序号回绕、32 包重排窗口、重复包、迟到包和连续缺口区间。
- H.264 重组支持单 NAL、STAP-A、FU-A；FU-A 缺起始、缺结束、缺中间片都会丢弃整帧。
- 报告输出完整帧数、丢帧数、关键帧数、总字节、前 3 帧 SHA-256 前 16 位，以及重复包、
  迟到包和序号缺口。

## 常用参数

```text
--transport tcp|udp        pull 使用的传输
--fixture auto             仅支持代码生成的确定性夹具
--frames N                 帧数（1..10000）
--loss/--reorder/--dup     损伤概率（0..1）
--seed N                   同一 seed 生成同一损伤序列
--out PATH                 原子写入 JSON 报告
--silence-timeout D        UDP 连续静默阈值
--max-reconnects N         UDP 静默后的重连上限
--server-policy strict|lenient
--session-timeout D        模拟 RTSP 会话超时
--stall-after N            模拟发送 N 包后停顿，用于验证重连
```

UDP 重连演示：

```sh
go run ./cmd/rtspaudit pull --transport udp --frames 120 --seed 7 \
  --stall-after 20 --silence-timeout 120ms --max-reconnects 2
```

## 退出码

| 码 | 含义 |
|---:|---|
| 0 | 流程完成；compare 两传输帧数一致且无非法迁移 |
| 1 | 内部错误 |
| 2 | RTP 缺口/丢帧/迟到包，或 compare 的 delta 非零 |
| 3 | 非法状态迁移、RTSP/RTP 解析失败、会话超时或重连耗尽 |
| 4 | 参数错误 |

## 独立模拟服务

```sh
go run ./cmd/mockserver --listen 127.0.0.1:0 --frames 120 --seed 7
```

服务会打印 `rtsp://127.0.0.1:PORT/audit`。也可以使用 `--policy`、`--session-timeout`
和 `--stall-after` 复现厂商差异与切网停顿。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

测试覆盖状态表、跨读 interleaved 半包、序号回绕/重排/重复/迟到、STAP-A/FU-A 重组、
TCP/UDP 端到端一致性以及 CLI 退出码。
