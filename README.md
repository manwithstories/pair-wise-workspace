# medpipe

本地低延迟媒体处理库与终端演示：把采集端的帧序列与 PCM 音频处理成可连续
传输的媒体单元，覆盖扫描校验、摘要索引、统一时钟、切分、静音检测、内容
指纹去重与 RTP 丢包恢复。纯本地运行，不联网、不访问系统设备、无 GUI。

## 技术栈

- Go 1.25.6（`go.mod` 的 `go` 指令固定为 1.25.6）
- `github.com/abema/go-mp4 v1.4.0`：MP4 媒体元数据解析
- `github.com/pion/rtp v1.8.25`：RTP 包构造与解析
- 标准库 `crypto/sha256`、`encoding/binary`、`testing`

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...
```

兼容 macOS arm64 与 amd64（`GOARCH=arm64|amd64 go build ./...` 均通过），
`go test -race ./...` 无数据竞争。

## 演示

正常场景（指标达标，退出码 0）：

```sh
go run ./cmd/medpipe demo --seed 42 --loss-burst 3 --latency-ms 80 --frames 240
```

损坏场景（中段帧损坏 + 尾部截断 + 不完整索引，退出码 1）：

```sh
go run ./cmd/medpipe demo --seed 42 --corrupt-frame 117 \
  --truncate-bytes 9 --index-incomplete
```

输出字段：`av_sync_error_ms`、`packet_recovery_pct`、`latency_p95_ms`、
`frames_indexed`、`truncated_tail_recovered`，以及切点、指纹、摘要等。

指标目标：同步误差 ≤ 20 ms、恢复率 ≥ 95%、P95 延迟 ≤ 120 ms。

退出码：`0` 完成且指标达标；`1` 输入损坏或指标未达标；`2` 参数错误。

## 架构

```
cmd/medpipe        参数解析与演示入口（库的薄封装）
internal/model     帧/音频块/时间戳、记录编解码、统一可定位错误
internal/scan      逐条扫描校验，区分可恢复尾损与中段损坏，构建摘要索引
internal/index     原子保存索引；摘要变化/字段缺失/中断临时文件触发重建
internal/clock     统一时间轴：回绕、重复序号、非单调时间与轨道停止
internal/segment   共享时钟固定窗切分，静音边界不改变视频切点
internal/audio     PCM 静音检测，输出起止时间与采样数
internal/fingerprint 规范化载荷指纹与去重，保留各自时间位置
internal/rtp       pion/rtp 构造、固定种子突发丢包/乱序与关键帧缺口判损
internal/pipeline  流式调度（内存缓冲上限 64 帧），汇总指标
internal/fixture   纯内存确定性样例生成与损坏注入
internal/metadata  go-mp4 元数据解析与最小 MP4 夹具
```

扫描子系统输出摘要绑定索引；索引校验输入摘要后交给执行流水线；流水线在
统一时间轴上依次调用切分、静音检测、指纹去重与 RTP 恢复。扫描与执行共享
时间基准和可定位错误模型。同一输入重复运行产生相同切点、指纹与摘要。
