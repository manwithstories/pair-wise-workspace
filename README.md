# archivectl — 影视冷存储时间戳修复与随机访问索引

纯本地、纯终端的 Go 1.25.6 工具。输入为本地生成/提供的 HLS/DASH 清单、fMP4
分片与 RTP 修复日志；输出为仅修正时间字段后的分片、确定性分块索引与独立核验
指标。**不转码、不改变媒体负载**，无任何外部服务访问。

## 快速验收

```bash
go version            # 必须输出 go1.25.6
go mod tidy
go mod download
go build -o "$TMPDIR/archivectl" ./cmd/archiver
go test ./...

WORK=$(mktemp -d "$TMPDIR/archive.XXXXXX")
"$TMPDIR/archivectl" demo   --root "$WORK" --seed 41 --segments 48 \
    --fault-profile boundary-v1 --resume-check
"$TMPDIR/archivectl" verify --root "$WORK" --repeat 2
```

也可直接运行 `scripts/acceptance.sh`。

`verify` 会分别打印 `clean` 与 `damaged` 两组 JSON 指标：

| 字段 | 含义 / 约束 |
| --- | --- |
| `av_sync_max_ms` | 音视频共享段边界的最大偏差（有理数精确换算，≤ 1.0ms） |
| `loss_recovery` | XOR FEC 恢复的丢包 / 总丢包（damaged 为 28/30 ≈ 0.9333，≥ 0.90） |
| `segment_p95_ms` | 独立重扫分片耗时 P95（远低于 40ms） |
| `index_sha256` | 规范化索引的 SHA-256，64 位小写十六进制 |
| `repeat_match` | `--repeat N` 次确定性重建摘要完全一致且等于已保存索引 |
| `digest_invalidated` | 输入增/删/改后整树 SHA-256 必然变化（在一次性副本上探测） |
| `unrecovered_gaps` | 无法恢复并显式留档的连续缺口数（clean=0，damaged=1，双包丢失） |

退出码：`0` 通过；`2` 参数错误；`3` 不可恢复结构损坏（如截断 trun）；
`4` 核验失败（指标/不变量不满足或输出被篡改）。

## 代码结构

```
internal/clock      跨模块共用的有理数时间基准与精确 rescale
internal/fault      统一携带 轨道/偏移/可恢复等级 的错误记录与退出码映射
internal/manifest   HLS（discontinuity/map/media-sequence）与 DASH
                    （SegmentTemplate/SegmentTimeline/timescale/PTO）解析；
                    本地路径安全边界；声道布局/采样率/色彩元数据调和
internal/scan       moof/tfhd/tfdt/trun 与同步样本扫描（go-mp4 解码），
                    非零起点、32 位回绕、截断盒定位
internal/repair     RTP 序号重排/去重/16 位回绕/旁带 XOR 恢复；
                    有理数时间线：DTS 单调、PTS-DTS 相对关系不变、歧义断点隔离
internal/index      所有输入字节的整树 SHA-256、确定性分块/样本边界索引、
                    临时文件 + rename 原子落盘
internal/execute    固定 32KiB 窗口流式拷贝 + 同宽 tfdt 原地补丁，
                    原子发布、完整块跳过的断点续跑、临时半成品清理
internal/audit      独立重扫输出、RTP 重放、随机跳转点对输入样本边界与
                    负载字节、摘要失效探测、指标与阈值
internal/layout     统一磁盘布局
internal/demo       确定性 clean/damaged 夹具生成与续跑演练
internal/pipeline   分析→修复→索引→执行编排
cmd/archiver        仅做命令行适配（demo / verify / scan-file）
```

## 关键不变量

- **安全**：拒绝任何带 scheme 的远端 URI、协议相对 URI、绝对路径与 `..` 越界。
- **时间**：视频 90kHz、音频 48kHz 都能在 `CommonTimescale=720000` 上无余数
  精确换算，因此对齐边界的 AV 偏差为 0；所有浮点只出现在指标展示层。
- **元数据**：仅当声道数唯一对应一种标准布局时补全（1→mono，2→stereo）；
  6 声道等多义情形或多来源冲突只写入 `metadata_conflict`，不伪造标签。
- **负载不变**：执行器只改写同宽 tfdt 值，盒大小与样本偏移不变；audit 在
  随机样本边界上逐字节比较输入与输出的样本负载。
- **确定性**：索引不含墙钟时间、随机 map 序；相同输入与输出得到相同
  `index_sha256`，重复运行/重复重建均 `repeat_match=true`。
- **内存有界**：哈希、拷贝与复制探测均使用固定窗口，增长的是索引条目而非
  单片缓冲；截断/损坏位置进入统一 fault 记录，结构损坏分片被隔离而不猜测。

## 自生成夹具（seed=41, boundary-v1）

damaged 组包含：两处清单 discontinuity 与 tfdt 时钟重置、视频 32 位 tfdt
回绕、RTP 序号 65535→0 回绕、重复包与乱序、28 个可恢复单包丢失（XOR）、
一个 FEC 组内双包不可恢复丢失（留档为 1 个含 2 包的缺口）、截断 trun
结构夹具、以及 6 声道却标 stereo 且采样率来源冲突的元数据夹具。
