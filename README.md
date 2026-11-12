# 多源事实表对齐管道（pair-wise ETL）

把三家供应商 CSV、一条业务线 JSONL 增量、内部结算库 SQLite 表合并成一张统一事实表，
全过程声明式配置、批式流处理、失败可定位（列 / 规则 / 原始值）、幂等可重跑。

## 安装

```bash
pip install -r requirements.txt   # pandas 2.2.2 / PyYAML 6.0.1 / pytest 8.2.2
```

## 一键验收

```bash
python scripts/acceptance.py --seed 7
```

期望输出（节选）：

```
SCHEMA target_fields=24 sources=3 bindings=61
FIELDS mapped=21 renamed=9 defaulted=3 dropped=2 conflicts=2
CONVERT failures=14 rows=11 reasons invalid_date=5 enum_unmapped=4 overflow=2
LOAD rows_in=1200 unique=1138 dup_resolved=62 loaded=1127 rejected=11
IDEMPOTENT checksum_pass1=xxxx pass2=xxxx equal=true
QUARANTINE file=run/quarantine.csv
EXIT 2
```

## 命令

```bash
export PYTHONPATH=src
python -m etl.cli align  --config config/mapping.yaml [--policy fail-fast]
python -m etl.cli verify --config config/mapping.yaml [--replay run/quarantine.csv]
python -m etl.cli report --run-dir run
python scripts/gen_fixtures.py --seed 7     # 重新生成夹具
python -m pytest tests/ -q                  # 单元与端到端测试
```

- `align`：config → registry 取连接器 → 按批读取 → plan 列映射与类型裁决 →
  coerce 转换（坏行隔离）→ staging 幂等合并 → reporter 落盘。
- `verify`：只校验映射规格与源 schema，打印冲突 / 默认值 / 丢弃列 / 缺失列。
- `report`：读取 `run/report.json` 输出人类可读汇总。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 全部成功，无隔离行 |
| 1  | 配置错误 / 源错误 / fail-fast 转换失败 |
| 2  | 存在隔离行（quarantine 模式） |
| 3  | 存在无法裁决的 unmerged 主键 |

## 稳定原因码（可 grep）

`invalid_date`、`enum_unmapped`、`overflow`、`not_a_number`、
`array_rejected`、`missing_pk`。

## 关键设计

- **声明式映射**（`config/mapping.yaml`）：24 个目标字段、3 个源、61 条绑定；
  未绑定字段记 `defaulted` 并填默认值，未使用源列记 `dropped`。
- **类型裁决**：跨源声明类型按 decimal>float>int>datetime>string 收敛；
  跨族（如 datetime vs int）记 `conflict` 并在报告中保留双源样本值。
- **转换阶梯**：空串 / NA / NULL 三态分别计数；支持千分位、括号负数、
  全角数字、科学计数法；金额分→元单位换算；枚举字典映射。
- **合并规则**：跨源同主键按 `priority` 取优，同源按 `updated_at` 取新，
  完全重复记 `dup_resolved`，同键同时间不同内容记 `unmerged`（退出码 3）。
- **幂等**：staging 表承接当批数据，目标表整体重建，重复运行行数与
  校验和不变；staging 表在 finally 中无条件清理。
- **隔离**：坏行写 `run/quarantine.csv`，附 `source/column/reason/raw_value/pk`，
  可用 `verify --replay` 经连接器层回放。
- **内存/性能**：默认 5 万行一批；实测 20 万行单次运行约 5 秒、峰值 RSS 约 300MB。

## 目录

```
config/mapping.yaml        映射规格（目标 schema / 绑定 / 枚举 / 失败策略）
src/etl/cli.py             align / verify / report 子命令
src/etl/config.py          规格校验（错误带行号）
src/etl/connectors/        base / csv / jsonl / sqlite / registry
src/etl/schema/plan.py     映射计划与类型冲突裁决
src/etl/transform/coerce.py 转换阶梯与原因码
src/etl/sink/sqlite_sink.py staging 幂等合并
src/etl/report/reporter.py 指标汇总与落盘
scripts/gen_fixtures.py    固定种子夹具生成
scripts/acceptance.py      端到端验收
tests/                     pytest 套件
run/                       运行产物（gitignore）
```
