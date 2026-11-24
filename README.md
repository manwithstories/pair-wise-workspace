# bcast — 广播级 CMAF/fMP4 批量核验与再封装流水线

纯 Go、纯终端、本地运行。样例分片由 `internal/fixture` 确定性生成，不访问网络素材。

## 验收命令

```bash
go mod tidy
go build ./...
go test ./...
go build -o /tmp/bcast ./cmd/bcast

/tmp/bcast demo --case clean --out /tmp/bcast-clean
/tmp/bcast demo --case truncated --expect-error --out /tmp/bcast-bad
/tmp/bcast demo --case drift-wrap-vfr --out /tmp/bcast-recover --fail-after index; echo exit=$?
/tmp/bcast resume --out /tmp/bcast-recover
/tmp/bcast resume --out /tmp/bcast-recover
```

退出码：

- `0`：完成，或 `--expect-error` 下确认了预期损坏样例。
- `2`：MP4/时间线等输入损坏。
- `3`：文件系统或 WAL 故障。
- `75`：`--fail-after` 注入中断。

也可运行 `scripts/acceptance.sh` 一键复现。

## 模块

- `internal/media`：有理时基、安全刻度转换、跨模块定位错误模型。
- `internal/boxes`：用 `github.com/abema/go-mp4` 读取盒头，扫描 `init/moof/mdat`，并发有界、流式摘要；识别缺号、乱序、重复 MFHD 和内容摘要重复。
- `internal/clock`：用 `github.com/pion/rtp` 解析 RTP 锚点，展开 32 位回绕、估计 PPM 漂移、融合 `tfdt` 与 `trun`，保持可变时长和组成时间偏移并生成单调 DTS。
- `internal/index`：生成 HLS manifest、`sidx`、`tfra`，校验随机访问偏移和 manifest/媒体时间偏差。
- `internal/metrics`：码率偏差、样本时长分位数、同步间隔、缺片/重复数。
- `internal/txn`：JSONL WAL、暂存代次、原子 `COMMIT.json`、SHA-256 校验、续跑和回滚。
- `internal/pipeline`：串接以上阶段。
- `internal/fixture`：生成 clean、truncated、duplicate/gap、drift-wrap-vfr、commit-interrupt 样例。

## 事务目录

```text
<out>/input/             # 自生成样例
<out>/wal.log            # fsync 的阶段记录
<out>/staging/gen-0001/  # 未提交暂存产物
<out>/committed/gen-0001/
<out>/COMMIT.json        # 原子发布的 SHA-256 提交标记
```

提交前会校验所有索引点和 manifest 偏差；提交后 resume 会重算 SHA-256。媒体载荷只做流式读取/摘要，不转码、不改写样本字节。
