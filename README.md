# sidxcheck

纯 Python 标准库实现的 fMP4 分片目录检查与 `sidx` 重建工具。扫描、样本表校验和索引重编码都作用于同一份解析结果，不依赖播放框架或媒体解析库。

## 快速验收

```bash
python -m sidxcheck gen --out fixtures/mixed --segments 12 --writer mixed
python -m sidxcheck scan fixtures/mixed
python -m sidxcheck rebuild fixtures/mixed --out rebuilt.sidx --report rebuilt.json
python -m sidxcheck verify fixtures/mixed --json verify.json
```

核心输出：

```text
sidx_entries=12 boundary_count=12 sample_count=3840 duration_ms=48000
rebuilt_boundaries=12 max_drift_ticks=0
offset_mismatch=2 offset_max_drift=128 max_drift_ticks=400 fatal_count=1
offset_mismatch index=5 start_delta=0 end_delta=-128 ...
offset_mismatch index=6 start_delta=-128 end_delta=0 ...
```

`mixed` 夹具同时包含两条互相抵消的坏 `referenced_size`、一个与首片 `tfdt` 不一致的 `earliest_presentation_time`，以及一个损坏的 `moof` 头。后者会记录 fatal，但解析器会重同步并继续处理后续分片。

## 命令

```bash
# 正常、缺索引、坏计数、截断、混合
python -m sidxcheck gen --out fixtures/normal --segments 12 --writer normal
python -m sidxcheck gen --out fixtures/missing --segments 12 --writer missing
python -m sidxcheck gen --out fixtures/bad_counts --segments 12 --writer bad_counts
python -m sidxcheck gen --out fixtures/truncated --segments 12 --writer truncated

python -m sidxcheck scan <文件或目录> [--json report.json]
python -m sidxcheck rebuild <文件或目录> --out rebuilt.sidx [--report rebuilt.json]
python -m sidxcheck verify <文件或目录> [--json verify.json]
```

目录会递归扫描；隐藏文件以及 `.json/.sidx/.txt/.md/.py/.log` 侧车文件会跳过。媒体文件扩展名不受限制。

退出码：

- `0`：全部一致。
- `1`：索引、偏移、时间线或样本表存在不一致。
- `2`：参数、路径或 IO 错误。
- `3`：返回降级的部分结果，至少有一条 fatal 结构错误。

## 模块

| 模块 | 职责 |
| --- | --- |
| `boxes.py` | 32 位、64 位、`size=0`、`uuid` 盒解析；容器递归；损坏后重同步 |
| `sidx.py` | `sidx` v0/v1 解码、引用标记、时长累计与重编码 |
| `moof.py` | `mfhd/traf/tfhd/tfdt/trun` 扫描、数据偏移和样本默认值 |
| `stbl.py` | `stts/stsc/stsz/stco/co64/stss` 与样本到 chunk 映射 |
| `rebuild.py` | 按实测 `moof` 起点、边界和时长生成新 `sidx` |
| `verify.py` | 复用扫描结果，校验偏移、计数、时间线、样本表 |
| `report.py` | 固定字段 JSON 和终端摘要 |
| `fixtures.py` | 现场生成所有二进制样例，无外部素材 |
| `cli.py` | `gen/scan/rebuild/verify` 装配与退出码 |

## 报告字段

JSON 始终包含 `sample_count`、`sidx_entries`、`boundary_count`、`offset_mismatch`、`max_drift_ticks`、`duration_ms`、`fatal_count`。同时提供：

- `offset_max_drift` / `offset_max_drift_index`：最大字节偏差及条目下标。
- `offset_mismatches`：每条起止偏移、实际值和字节差。
- `fatal`：文件、偏移、错误码和消息。
- `validation_errors`：计数、时间线、stsc/stts 等交叉校验问题。

`verify` 的 `max_drift_ticks` 取 `sidx` 时长/时间戳、相邻 `tfdt`、`stts` 分块累计时长之间的最大偏差；`rebuild` 报告的是重建条目与实测 `moof` 时长的最大偏差。

## 防御边界

- 默认最大递归深度 32 层。
- 单个 box 默认上限 1 GiB，单文件上限 16 GiB，超限不会按声明尺寸分配内存。
- 顶层 box 损坏时最多向前搜索 16 MiB 进行重同步。
- `trun` 与样本映射均限制单次 2,000,000 项，避免恶意计数放大。
- 100,000 样本映射按线性时间完成，并有回归测试约束在 5 秒内。

## 测试

```bash
python -m pytest -q
```

样例文件由 `gen` 现场生成；`fixtures/`、`rebuilt.sidx` 和报告文件默认不进入版本库。
