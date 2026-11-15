# m3u8ctl

纯本地运行的 HLS 清单生成与分片校验工具（Go 1.22，仅标准库，无外部二进制）。
面向多码率直播回看场景：四档码率分片落在同一目录，链路存在重传、乱序、丢片，
工具负责重建四档时间轴、生成 master/media 清单，并用同一自研解析器读回做往返校验。

## 目录结构

- `cmd/m3u8ctl` — `build` / `verify` / `fixture` 子命令入口与退出码映射
- `internal/segment` — 目录扫描、文件名解析、乱序缓冲去重、时间轴索引与缺口/重叠判定
- `internal/playlist` — 清单内存模型、原子写入器、自研解析器、索引→清单构建
- `internal/verify` — 校验规则集、跨档对齐、稳定 JSON 报告
- `internal/fixture` — 确定性生成带丢片/乱序/重传的样例分片

## 分片命名约定

```
r<档位>-s<6位序号>-t<13位源时间戳ms>-d<声明时长ms>[.rt<重传序号>].ts
例: r2-s000017-t0000000068000-d4000.rt3.ts
```

非法文件名计入报告 `failures`（`bad_name`），不中断扫描。同档位同序号多次到达
取末次（`.rt` 序号更大者）胜出，并累加 `duplicate_conflicts`。

## 数据流

```
fixture 产出目录
  -> segment.ScanDir   解析/哈希/去重为分片记录
  -> segment.BuildIndex 建立四档时间轴、缺口区间、漂移/重叠/零字节
  -> playlist.BuildFromIndex + WriteMedia/WriteMaster 原子落盘
  -> playlist.ParseFile 读回（与 writer 同一套标签）
  -> verify.Checker     与源索引逐项比对 + 跨档对齐
  -> verify.Report      stdout 或落盘 JSON，终端摘要走 stderr
```

清单通过 `#EXTM3U8CTL-*` 自定义标签携带序号、源时间戳、实测/声明时长等元数据，
标准 HLS 播放器会忽略未知标签；本工具解析器据此完成往返一致性判定。
缺失序号连续补为 `#EXT-X-GAP` 段（URI 形如 `gap-r0-s000003.ts`），并在索引中
单独输出缺口区间。

## 演示

```
go run ./cmd/m3u8ctl fixture --out ./work --renditions 4 --segments 240 --drop 7 --shuffle --seed 42
go run ./cmd/m3u8ctl build   --src ./work --out ./out --target-duration 4 --window 0
go run ./cmd/m3u8ctl verify  --playlist ./out/master.m3u8 --src ./work --drift-ms 50 --out report.json
```

报告顶层字段（脚本可稳定依赖）：

```
{"renditions":4,"segments":233,"gaps":7,"gap_intervals":7,"duration_s":960,
 "target_duration":4,"drift_max_ms":38,"round_trip_ok":true,
 "duplicate_conflicts":12,"failures":[],"rendition_stats":[...]}
```

`duration_s` 是完整源时间轴时长（`(最大序号+1)*目标时长`），不因丢片缩短；
`segments` 为单档实际分片数，`gaps` 为单档缺口槽位数（四档共享同一序号网格）。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 全部通过（无缺口、无失败、往返一致） |
| 1  | 存在缺口或任一校验失败（drift/zero_byte/size_mismatch/overlap/...） |
| 2  | 参数错误或目录/文件不可读 |
| 3  | 清单与源索引往返不一致，或清单无法解析 |

## 其他能力与约束

- `--window N` 滑动窗口：只保留最近 N 个分片并推进 `EXT-X-MEDIA-SEQUENCE`，`0` 为全量。
- 所有清单写入走“同目录临时文件 + fsync + rename”，进程中断不留半截文件。
- 实测时长由 `分片大小*8000/码率(bps)` 推导（无 ffmpeg 依赖）；码率均为 8000 的
  倍数，毫秒计算无截断误差。漂移超 `--drift-ms` 记 `drift`，相对偏差超 10% 另记
  `size_mismatch`，零字节单独记 `zero_byte`。
- 解析器对任意输入不 panic：worker 内 `recover` 兜底，异常一律转 failures。
- 作用域仅限当前目录与显式指定的输出目录；无第三方模块（`go.mod` 无 require）。
- 基线吞吐：4000 片 build≈0.3s、verify≈0.27s（远超每百片 1 秒要求，机器不同会有差异）。
