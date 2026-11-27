# archiver — RTP/RTCP 档案修复管线

将老采集设备留下的带丢包、乱序的 RTP/RTCP 包流还原为可追溯的音视频
时间线，统一音频规格，建立可随机访问的视频关键帧索引，并按时间片
归档、独立核验。纯终端形态，无外部服务依赖。

## 构建与测试

```sh
brew install ffmpeg pkgconf
export PKG_CONFIG_PATH="$(brew --prefix ffmpeg)/lib/pkgconfig"
go mod tidy && go mod vendor
go build ./... && go test -race ./...
```

## 验收演示

```sh
go run ./cmd/archiver demo --case clean --seed 73
go run ./cmd/archiver demo --case damaged --seed 73 --inject truncated-extension,duplicate,wrap,late-vfr
go run ./cmd/archiver demo --case recovery --seed 73 --interrupt-after 3 --out "$TMPDIR/archive-demo"
go run ./cmd/archiver resume --out "$TMPDIR/archive-demo"
```

退出码：0 断言通过；1 内部不变量或哈希不符；2 参数错误或不可恢复
输入；75 已按参数保存检查点并等待续跑。

## 架构

数据流：RTP/RTCP 解析与有界重排 → 共享时钟模型 → 时间线水位与确定
性回放 → 音频归一化 / 视频关键帧索引 → 分片归档与独立核验。

| 模块 | 职责 |
| --- | --- |
| `internal/media` | 微秒时间基准、轨道标识、可恢复错误 |
| `internal/rtp` | 长度前缀报文增量解析（首错字节偏移）、128 包窗口重排 |
| `internal/clock` | RTCP SR 校时、漂移最小二乘拟合、32 位回绕扩展 |
| `internal/timeline` | 水位推进、检查点、迟到回溯重放与隔离 |
| `internal/audio` | 44.1k 单声道 → 48k 立体声，-18 LUFS / ≤-1 dBTP |
| `internal/video` | VFR 时间戳保留的关键帧索引与解码核对 |
| `internal/archive` | 2 秒时间片原子落盘、SHA-256 清单、检查点 |
| `internal/fixture` | 确定性样例流生成与六类故障注入 |
| `internal/app` | 端到端管线装配 |
| `cmd/archiver` | 终端入口（demo / resume） |

## 可注入故障

`short-header`、`truncated-extension`、`duplicate`、`wrap`、`reorder`、
`loss`、`late-vfr`、`bad-frame`、`drift-no-rtcp`。

相同输入、参数与种子的重复处理必然得到相同的片段 SHA-256。
