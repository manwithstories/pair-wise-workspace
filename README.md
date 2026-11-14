# mkvtool

本地 Matroska 检查、时间戳修复与 MP4 重封装工具。EBML 解析、样例生成和 MP4 box 写出均为仓库内自行实现，不依赖现成 MKV/MP4 封装库，也不做编解码或滤镜。

## 快速验收

```bash
python -m mkvtool gen-fixtures --out fixtures --cases 7
python -m mkvtool probe fixtures/unknown_segment.mkv --json out/tree.json
python -m mkvtool remux fixtures/negative_dts_b.mkv -o out/neg.mp4 --report out/neg.report.json
python -m mkvtool verify out/neg.mp4 --report out/neg.report.json
```

基准夹具结果：

- `unknown_segment.mkv`: `element_count` 约 4235，`unknown_size_elements` 9，`max_depth` 6。
- `negative_dts_b.mkv`: 2 条输出轨、1500 个视频帧、首帧 DTS `-80 ms` 平移为 `0 ms`、38 个关键帧。
- `verify`: `frame_count_match`、`dts_monotonic`、`index_matches` 均为 true；另输出 `timestamps_match`。

## 子命令

```bash
python -m mkvtool gen-fixtures --out DIR [--cases N]
python -m mkvtool probe INPUT [--json PATH]
python -m mkvtool remux INPUT -o OUTPUT [--report PATH] [--gap-ms MS] [--fill-gaps] [--no-shift]
python -m mkvtool verify OUTPUT --report PATH
```

`probe` 与 `remux` 都只解析输入一次；元素树摘要、轨道映射、时间线和 Muxer 共用同一份解复用结果。`remux` 落盘后会重新打开 MP4 解析其 `stbl` 表并自校验，失败时以退出码 5 结束。

## 退出码

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功，片段被截断但已降级为可用前缀时仍为 0 |
| 2 | EBML/Matroska 输入解析失败，包括损坏头、VINT、越界长度 |
| 3 | 没有可写入 MP4 的受支持轨道 |
| 4 | 时间戳不可修复，例如 `--no-shift` 下仍有负 DTS |
| 5 | MP4 自校验或 `verify` 报告比对不一致 |

## 实现要点

- VINT 覆盖 1–8 字节 size 与 1–4 字节 ID；未知 length 使用全 1 payload 标记。
- 未知长度 Segment/Cluster 利用父级允许的兄弟元素 ID 定界；未知长度链上的 EOF 截断降级，并记录 offset 与原因。
- 时间线按全片段统一平移，保持音画相对关系；同轨 DTS 严格递增，空洞超过阈值时报告，视频可重复上一帧填静帧。
- 无 Cues 时从关键帧扫描生成 `stss`；有 Cues 时校验时间和 Cluster 偏移，MP4 索引始终由实际关键帧重建。
- MP4 写出 `stts/ctts/stss/stsc/stco/stsz`，chunk offset 指向真实 `mdat` 数据；AVC/AAC sample entry 使用源 `CodecPrivate`。
- 不支持的轨道和 lacing 等异常块会进入报告的 `skipped_tracks`/`demux_issues`，不阻断其他轨道。

## 夹具与限制

夹具是确定性合成数据：容器结构、时间戳、索引和 sample table 均可被本工具及 `ffprobe` 识别，但视频/音频 payload 是占位数据，不保证可解码播放。真实素材重封装仍只搬运原始 sample payload。

B 帧夹具的 presentation offset 使用标准 `BlockAdditions` 携带内部元数据 `PTSOFF:<ms>`；实际 MP4 端从该偏移生成 `ctts`。未携带此扩展的真实 AVC 输入按块时间戳作为 DTS 处理。

## 测试

```bash
python -m pytest -q
```

测试覆盖 VINT 全宽度、未知长度定界、截断降级、轨道/块抽取、负 DTS 平移、空洞填充、MP4 table 自解析，以及 CLI 退出码 2/3/4/5。
