# m3u8ctl

面向本地赛事时移目录的 HLS 切片、清单生成与一致性校验工具。全部样例 TS、异常数据和报告均由程序生成，不依赖外部服务或媒体文件。

## 构建与验收

要求 Go 1.22 及以上；依赖 `golang.org/x/sync v0.7.0` 已位于 `vendor/`，离线构建无需下载。

```sh
go build ./...
go run ./cmd/m3u8ctl fixture --out testdata/fx --scenario gap-reorder --renditions 3 --segments 30 --seed 7
go run ./cmd/m3u8ctl gen --input testdata/fx/live --out testdata/fx/hls --window 10 --target-dur 2 --endlist
go run ./cmd/m3u8ctl verify --root testdata/fx/hls --format table --tolerance-ms 120
go run ./cmd/m3u8ctl verify --root testdata/fx/hls --format json --out report.json
```

`gap-reorder` 产生 4 类核心异常并返回退出码 1；`clean` 场景无失败并返回 0。参数、目录或清单入口错误返回 2；未预期 panic 被恢复后返回 3。

## 子命令

```text
fixture --out DIR --scenario NAME --renditions N --segments N --seed N
gen     --input DIR --out DIR --window N --target-dur SEC --endlist
verify  --root DIR --format table|json --tolerance-ms N --out FILE
```

`fixture` 场景：`clean`、`gap-reorder`、`dup`、`truncated`、`empty`、`pcr-rollback`。

## 行为

- `gen` 已按 PCR 时间排序，文件名和 mtime 只用于稳定平局排序，不决定时间轴。
- `--window N` 仅保留最新 N 片，裁掉旧片时递增 `MEDIA-SEQUENCE`。
- 相邻 PCR 间隔超过目标时长 1.5 倍时写入 `EXT-X-DISCONTINUITY` 并递增断点序号。
- `EXTINF` 由首末 PCR 加一帧间隔计算并由 `probe` 实测；偏差超过阈值记 `EXTINF_DRIFT`。
- `BANDWIDTH` 取完整 3 片滑窗峰值，`AVERAGE-BANDWIDTH` 取总字节除以总时长，均向上取整到 bit/s。
- 校验逐段单遍扫描，不整文件读入；1200 片只保留每段摘要，内存随片数线性增长且远低于 256 MB。

## 报告

表格每档一行：标称/实测码率、应有/实到片数、最大/平均时长偏差、失败数。JSON 使用固定字段、固定档位顺序和稳定失败排序，可直接 diff。

失败码包括 `SEG_GAP`、`SEG_DUP`、`SEG_ORDER`、`SEG_EMPTY`、`TS_TRUNCATED`、`PCR_MISSING`、`PCR_ROLLBACK`、`PCR_JUMP`、`EXTINF_DRIFT`、`BANDWIDTH_MISMATCH`、`VARIANT_SKEW`、`SEGMENT_COUNT_MISMATCH`、`DISK_EXTRA` 和 `PLAYLIST_INVALID`。连续缺失引用合并为一条 `SEG_GAP`，并带首尾序号。

## 目录

```text
cmd/m3u8ctl/          CLI 与退出码
internal/playlist/    属性词法、清单编解码、滚动窗口
internal/ts/          188 字节包、PCR、IDR 关键帧与时长探测
internal/slicer/      三档合成与并发切片
internal/fixture/     故障场景注入
internal/gen/         排序、取窗、复制与码率生成
internal/verify/      磁盘核对、跨档对齐与报告
```
