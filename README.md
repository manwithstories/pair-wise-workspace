# tsprobe

`tsprobe` 面向地方台、户外大屏和素材入库场景，对单文件 MPEG-TS 做流式诊断与切片。它只读取本地 `.ts` 并写本地 JSON/TS 文件，不启动服务、不访问网络。

## 构建

```sh
go build ./...
```

Go 版本要求 1.22。TS 包与 PES 读取使用 `github.com/asticode/go-astits`；PAT、PMT、SDT 的 section 重组、MPEG CRC、版本状态机和 PID 映射均由本项目自行实现。

## 演示

```sh
go run ./cmd/tsprobe gen --out testdata/room.ts --programs 3 --switch 4 --drop 0.01 --seed 7
go run ./cmd/tsprobe scan --input testdata/room.ts --json out/scan.json
go run ./cmd/tsprobe slice --input testdata/room.ts --segment-ms 2000 --out-dir out/seg --index out/index.json
```

`gen` 的参数：

- `--programs`：节目数，默认 3。
- `--switch`：中途节目表版本切换次数；各 PMT 每次切换版本递增，PAT 在切换点递增；第二次切换会交换两个节目的 PMT PID，便于检查“节目被顶替”的映射快照。
- `--drop`：短突发丢包强度，0 表示不丢包；默认按 3–5 个连续空口包注入，便于观察 CC 跳变与 corrupt 分片。
- `--seed`：随机种子；相同参数生成逐字节相同的 TS。

夹具是结构合规的合成 TS，包含真实 PAT/PMT/SDT、PES 头、PTS 与 H.264 风格关键帧标记，用于诊断和切片链路验证，不包含可解码的真实画面素材。

## scan 输出

`scan` 单遍扫描文件，终端打印摘要，`--json` 写出稳定字段顺序的 JSON。主要字段：

- `packets_total`、`size_bytes`、`duration_ms`。
- `pat`、`pmt[]`：版本变化次数、首末版本、每次变化的包序号/字节偏移、映射快照与 stale 状态。
- `streams[]`：按 PID 升序，含 `packet_count`、`payload_bytes`、重复包与估算丢失包。
- `null_pid`：null PID 单独统计。
- `cc_gaps[]`：连续计数跳变的起止包、期望/实际 CC 与缺失包数。
- `anomalies`：CRC、同步丢失、全零段、尾部残包、长度非 188 整数倍等计数。
- `mapping_history`、`sdt`：节目映射时间线与当前 SDT 服务。

同一文件重复运行 `scan`，JSON 逐字节一致。扫描只保留当前 188 字节包、有限大小 PSI section 和稀疏事件，不缓存整文件。

## slice 输出

`slice` 复用同一次 `demux.Scan` 的 `psi.Store`、PID 归属与丢包结果。切点取自 `payload_unit_start` 的视频关键帧；节目表版本变化处强制断片。跨 CC 丢包的分片标记 `corrupt`，并在 `playable_ranges` 中给出仍可播放的包区间。

`index.json` 顶层的 `slices.total` 与 `slices.corrupt` 可直接用于入库前筛选。TS 分片按源文件包序重新落盘，保持原始 CC，不做重写或滤镜。

## 异常与退出码

- `0`：无异常。
- `2`：报告完整，但存在丢包、版本跳变、stale 或其他可恢复异常。
- `3`：输入无法识别或读写失败。
- `4`：参数非法。

注意：`go run` 会把非零子进程状态统一包装为工具自身的退出码 1，并打印 `exit status N`。需要按上述数字验收退出码时，请运行 `go build` 产生的二进制。

同步丢失时会寻找下一个可信同步点继续；全零填充段和尾部残包只计数，不导致 panic。CRC 错误的 PAT/PMT 被丢弃，上一有效映射继续使用并标 `stale`。

## 自测

```sh
go test ./...
```

测试覆盖 section 跨包拼接/CRC、`current_next=0` 与版本去重、stale 回退、夹具端到端扫描、切片确定性和异常文件重同步。
