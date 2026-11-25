# FieldCut

FieldCut 是一个纯本地、确定性的音乐节后期片段包生成器。它把导演机位、舞台机位、RTP/MP4 样本、PCM 分轨以及字幕/ID3/emsg/SEI 事件统一到纳秒半开区间时间线，完成对齐、拓扑排序、音频分析、切点规划、内容去重、清单写出和可恢复提交。

## 快速验收

```bash
go mod tidy
go build -o "$TMPDIR/fieldcut" ./cmd/fieldcut
go test ./...

OUT="$(mktemp -d)"
go run ./cmd/fieldcut demo --case clean --seed 17 --out "$OUT/clean"
go run ./cmd/fieldcut verify --out "$OUT/clean"
go run ./cmd/fieldcut demo --case damaged --seed 17 --out "$OUT/damaged"
go run ./cmd/fieldcut demo --case interrupted --seed 17 --out "$OUT/recover"
go run ./cmd/fieldcut resume --out "$OUT/recover"
```

也可以运行 `./scripts/acceptance.sh`。该脚本直接检查构建产物的退出码；请注意 `go run` 会把任意非零子程序退出码统一打印为 `exit status N` 且 `go` 命令自身返回 1，精确的 2/75 退出码由编译后的二进制保留。

## 退出码

- `0`：成功并通过指标约束。
- `2`：输入损坏；已写出可保留片段、清单和首个错误区间。
- `3`：配置、路径或写入/码率校验失败。
- `75`：可恢复的模拟中断；阶段日志已落盘。

## 关键约束

- 所有区间为半开区间 `[start_ns, end_ns)`。
- 错误模型包含稳定 `error_code`、`track_id`、原始时间戳/原始值和可选区间。
- RTP 16/32 位回绕、乱序、重复和缺包在 `internal/ingest/rtp.go` 处理。
- MP4 读取使用 `github.com/abema/go-mp4/v1.7.2` 的 box 解析能力，并校验声明长度和越界偏移。
- PCM 支持 44.1/48 kHz、mono/stereo/5.1 与目标布局映射、RMS/peak/削波和可配置静音检测。
- 指纹先比较廉价哈希，再逐样本比较 SHA-256 摘要，防止哈希碰撞误去重。
- 色彩冲突必须由 `strict` 或 `prefer_track` 显式策略处理，决策写入 `manifest.json`。
- 阶段日志记录输入哈希；输出使用同目录临时文件、`fsync` 和原子 rename。
- 所有夹具由固定 seed 在代码中生成，不读取外部媒体文件。
