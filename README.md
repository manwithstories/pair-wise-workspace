# forensictl

纯 Python 标准库的离线数字取证演示流水线。项目从空仓生成自有 raw 镜像，完成块级哈希链校验、SQLite freelist/WAL 离线恢复、NTP 与 DST 时间归一、跨源事件合流、空档扫描和可复核报告；运行期除 `pytest` 测试外无第三方依赖。

## 快速验收

要求 Python 3.11+。仓库根执行：

```bash
python fixtures/make_image.py --out evidence --seed 20260308
python -m forensictl.cli run \
  --image evidence/evidence.img \
  --manifest evidence/manifest.jsonl \
  --work evidence/work.db \
  --bucket 300
```

默认镜像为 64 块、每块 4 MiB，合计 256 MiB。输出形如：

```text
events=1284
hash_mismatch=3
mismatch block=3 offset=12582912 asset=FILE_RECORDS logical_offset=0 ...
mismatch block=4 offset=16777216 asset=NTP_LOG logical_offset=0 ...
mismatch block=6 offset=25165824 asset=MAINTENANCE logical_offset=0 ...
missing_ranges=2
missing start=2026-03-08T00:05:00.000000Z end=2026-03-08T01:00:00.000000Z
missing start=2026-03-08T09:00:00.000000Z end=2026-03-08T09:55:00.000000Z
exit=1
```

同一命令重跑会命中工作库检查点，输出逐字相同。`make_image.py --blocks 16` 可生成 64 MiB 小样本，供测试快速运行。

## 命令与退出码

```bash
python -m forensictl.cli verify --image ID --manifest MANIFEST [--json PATH]
python -m forensictl.cli timeline --image ID --manifest MANIFEST --work WORK --bucket 300
python -m forensictl.cli report --work WORK [--json PATH]
python -m forensictl.cli run --image ID --manifest MANIFEST --work WORK --bucket 300 [--json PATH]
```

- `0`：完整性校验干净。
- `1`：存在哈希不一致、链断、块数不符、截断或短末块等完整性问题。
- `2`：输入、格式或工作库错误。

`--json -` 将 JSON 写到标准输出；`--json PATH` 写文件。报告中的事件数、不一致数、恢复数和空档数均从工作库查询计算。

## 完整性结论

`manifest.jsonl` 在注入三处静默改写之前生成。每行记录 `block`、物理 `offset`、`length`、该块 SHA-256 和前一行的 SHA-256；首行 `prev_hash` 为 64 个零。`ImageIO` 以只读、页对齐的滑动 `mmap` 窗口读取镜像，校验器每次只保留 1 MiB 数据块，不把 256 MiB 镜像整体驻留内存。

本样本中三个目标块分别属于 `FILE_RECORDS`、`NTP_LOG` 和 `MAINTENANCE`。因此结论不是“镜像与采集时完全一致”，而是：

- 清单可证明采集后至少三个块的内容发生变化；
- 报告给出块号、物理偏移、所属资产及资产内逻辑偏移；
- 后续时间线必须携带该完整性缺陷，不能把恢复结果表述为未经污染的原始状态；
- 哈希链本身未断时，仍可区分“清单链结构错误”和“块内容被改写”。

`verify` 同时检查清单行数与超级块声明块数、清单顺序/偏移、链式 `prev_hash`、镜像截断和末块短读；这些情况返回结构化错误，不触发未处理异常。

## 恢复与时间线方法

- `imageio.py` 以只读 `mmap` 打开镜像，从超级块读取分区表，按资产和块切片；越界显式报错，截断与空洞可零填充后安全返回。
- `sqlitecarve.py` 不把原始镜像交给 `sqlite3`。它定位内嵌主库与 WAL，校验 WAL 头/帧累计校验和，只应用最后一个提交点之前的帧；解析 b-tree 页、rowid 表、溢出页和 freelist，扫描已删行残留，按 rowid 去重后写入工作库，再由 `sqlite3` 查询。
- `timebase.py` 从镜像资产构造 TZif 并交给 `zoneinfo` 解释。带显式偏移的时间直接折算；naive 本地时间先按镜像时区解释，再按 NTP 阶跃和线性漂移分段校正。春季不存在时刻使用跳变前偏移，秋季重复时刻取首次出现。
- `filemeta.py` 解码 UTC/本地两套 100ns 时间戳，检测倒挂和晚于采集时刻，告警事件保留原始整数值。
- `timeline.py` 在实体键相同且时间差不超过 2 秒时合并跨来源事件，保留来源列表；排序键为 UTC、来源优先级、序号。负间隔会被钳制并标记 `anomalous`，超阈值点保留而非丢弃。
- 空档扫描按 300 秒对齐桶进行，合并连续空桶，再扣除镜像时区表的春季跳变窗口和已知维护窗口。

详细二进制布局见 [docs/format.md](docs/format.md)，取证流程与限制见 [docs/method.md](docs/method.md)。

## 测试

```bash
python -m pip install -r requirements-dev.txt
python -m pytest
```

测试覆盖三块篡改、清单块数不符、链断、镜像截断/短末块、freelist/WAL/溢出页恢复、未提交 WAL 帧丢弃、DST 不存在与重复时刻、两次 NTP 阶跃和线性漂移、元数据倒挂/未来时间、跨源去重、空档排除、检查点续做和重复运行幂等。

本地验收环境实测：256 MiB 首跑约 0.4 秒，进程峰值 RSS 约 43 MB；重复运行命中完整检查点并逐字比对输出一致。
