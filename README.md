# mediascan

纯本地、纯终端的 MP4/MOV 容器与样本表检查工具。它不解码音视频，只校验 box、样本表、样本字节范围、关键帧时间，并生成可追溯的规范化 JSON 报告。

## 快速验收

```sh
go mod tidy
go build ./...
go test ./...

go run ./cmd/mediascan demo --work ./.demo --workers 4 --seed 17
shasum -a 256 ./.demo/report.json
```

演示固定生成 6 个双轨素材（共 12 轨），覆盖：

- 普通 `stts/stsc/stsz/stco/stss` 样本表
- 分片 `moof/traf/tfdt/trun`
- 30000/24000/25000/48000/44100 等不同 timescale
- MP4 与 QuickTime `.mov` edit list
- 截断 box、mdat 声明越过 EOF、样本偏移越界、tfdt 回退
- 零 timescale
- 3 个任务已提交后的中断与续跑

成功输出包含：

```text
files=6
tracks=12
corrupt_regions=4
offset_error_max_bytes=0
keyframe_time_error_max_ticks=0
resume_skipped=3
resume_ms=<毫秒>
report_sha256=<64 位十六进制>
```

重复运行会重建相同的确定性中断现场；除 `resume_ms` 等不写入 `report.json` 的耗时字段外，报告内容和 SHA-256 保持一致。打印的 `report_sha256` 等于 `shasum -a 256 ./.demo/report.json`。

## 扫描本地素材

```sh
go run ./cmd/mediascan scan --work ./.scan --workers 4 /path/to/media-or-dir
```

退出码：

- `0`：扫描完成且无结构性损坏
- `1`：内部错误
- `2`：扫描完成但发现损坏或零 timescale
- `3`：收到取消信号，但已提交检查点可续跑
- `4`：素材 SHA-256、分析器版本或参数指纹与旧检查点不匹配

建议在需要严格判断退出码时使用编译后的二进制；`go run` 可能会把非零程序退出状态包装为其自身的运行错误输出。

## 设计约束

- 单文件上限：8 GiB
- box 最大深度：32
- worker 并发：1–16
- 所有读取均通过有界 `ReaderAt`/游标，避免越界读取
- 检查点键：`SHA256(素材哈希 + 分析器版本 + 参数指纹)`
- 检查点使用同目录临时文件、`fsync`、`rename` 和目录同步原子落盘
- 报告为结构化 JSON，文件、轨道和故障均确定性排序
