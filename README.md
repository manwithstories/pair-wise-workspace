# mediaqc

面向赛事与发布会素材交付的纯标准库封装检查器。它只读取容器和样本表元数据，不读取 `mdat` 媒体负载，也不调用 ffmpeg 或任何解码库。

## 环境与安装

- Python 3.11+
- 运行时无第三方依赖
- 测试依赖 pytest 8.1

```bash
python3.11 -m pip install -e ".[dev]"
```

如果本机命令名为 `python3`，将下文命令中的 `python3.11` 替换即可。

## 生成夹具

所有夹具均由脚本按字节生成，不引用外部媒体：

```bash
python scripts/make_fixtures.py --out fixtures --count 24 --seed 7
```

生成的 24 条示例覆盖干净 MP4/MOV/WAV/AVI、音画漂移、co64、编辑列表、负 ctts、空编辑、样本表不一致、stco 越界、块重叠、缺 SPS、moof、截断、尾部垃圾、零时基、无同步样本、多 SPS、奇数长度 RIFF 块等场景。`--count` 大于 24 时继续生成轻量批量文件。

## 命令

```bash
# 单文件探测
mediaqc probe fixtures/clean_h264_aac.mp4 --json-out reports/single.json

# 递归批处理；4 个解析进程，SQLite 单写线程
mediaqc batch fixtures --db work/media.db --jobs 4 --json-out reports/batch.json

# 查询 error 与 warning，并按规则号聚合
mediaqc check --db work/media.db --min-severity warning --json-out reports/findings.json

# 查看数据库总体情况
mediaqc report --db work/media.db --json-out reports/report.json

# 按同步样本和 4 秒目标生成分片清单
mediaqc pack fixtures/drift_audio.mp4 --target-ms 4000 --out-dir work/pack
```

`probe` 输出 `container`、轨道数、音画呈现时长、`drift_ms` 和发现数。`batch` 输出文件数、轨道总数及按严重级计数。`check` 输出命中总数、严重级计数和每条规则计数。`pack` 输出分片数与最大分片时长偏差，并生成 `manifest.json` 及逐片 JSON。

## 退出码

- `0`：没有 error 级问题
- `1`：存在 error 级问题，或 pack 的字节区间越界
- `2`：参数、文件或数据库 IO 错误
- `3`：封装不支持

## 解析与规则要点

- 嗅探支持带前置 `free/skip/wide` 块的 BMFF、64 位盒长度、`size=0` 的末尾盒，以及 RIFF AVI/WAVE。
- BMFF 读取校验盒边界、容器内尾随字节和最大 32 层嵌套；`mdat` 只跳读，不载入内存。
- 轨道时间线来自 `stts/ctts/stsc/stsz/stco/co64/stss`，呈现时长结合 `mdhd` 时基和 `elst` 电影时基计算。
- `avcC` 可列出同一轨道的全部 SPS 并通过 Exp-Golomb 解析 SPS；`hvcC` 解析参数数组；`esds` 解析 AAC AudioSpecificConfig。
- 规则覆盖音画漂移、样本数不一致、块越界/重叠、交错间隔、空编辑、零时基、负 ctts、缺参数集、多 SPS、无同步样本、截断与尾部垃圾等。
- 样本表超过 10 万项时只保留计数与表头事实，不展开逐样本时间线；元数据读取上限为 32MB，大表按受控前缀读取。
- SQLite 以规范化路径为幂等键，单文件事务内替换轨道与发现；所有查询值均使用 SQL 参数绑定。

## 测试

```bash
pytest
```

测试自动生成临时夹具，不依赖仓库外文件。
