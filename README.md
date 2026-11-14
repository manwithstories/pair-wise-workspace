# tlcore

面向过场动画排片的纯本地时间线内核与批量命令行处理器。内核不依赖
OpenTimelineIO：所有时间以 1/48000 秒整数采样表示，OTIO 只用于交换。

## 安装

```bash
python -m pip install -r requirements.txt
```

目标运行时为 Python 3.11，固定依赖 `opentimelineio==0.17.0`、
`PyYAML==6.0.1`、`pytest==8.2.0`。

## 快速验收

```bash
python -m tlcore.cli validate --in fixtures/ep01_v2.json --report out/v.json
python -m tlcore.cli resolve --in fixtures/ep01_v2.json --policy ripple,trim \
  --out out/r.v2.json --report out/r.json
python -m tlcore.cli schedule --in out/r.v2.json --fps 30000/1001 --report out/s.json
python -m tlcore.cli convert --in fixtures/ep01_v1.json --to v2 --out out/u.v2.json
python -m tlcore.cli otio --in out/r.v2.json --export out/e.otio
```

示例数据由代码生成，不引用外部素材：

```bash
python tools/gen_fixtures.py
python tools/gen_fixtures.py --pressure out/pressure_v2.json --pressure-count 50000
```

## 模块

- `tlcore/timebase.py`：有理数时间、毫秒/秒/帧/分数帧率转换、48 kHz 格点与逐半偶舍入。
- `tlcore/intervals.py`：半开区间相交、包含、相邻、合并和重叠簇。
- `tlcore/model.py`：`Timeline`、`Track`、`Segment`，优先级、锁定和 z-order。
- `tlcore/conflict.py`：扫描线同轨检测、非法区间分类、视觉遮蔽和音频并发统计。
- `tlcore/resolve.py`：`trim`、`ripple`、`spill`、`drop`、`fill`，支持轨道/簇作用域。
- `tlcore/etj_v1.py`：扁平片段、整毫秒 ETJ v1。
- `tlcore/etj_v2.py`：分层轨道、属性继承、相对偏移 ETJ v2。
- `tlcore/convert.py`：ETJ v1/v2 互迁与 OTIO 双向转换、规范摘要。
- `tlcore/schedule.py`：逐帧活动表、视觉遮蔽标记、音频并发和摘要哈希。
- `tlcore/cli.py`：终端参数、报告落盘和退出码。
- `tools/gen_fixtures.py`：固定种子式夹具与 5 万片段压力数据生成器。

## 时间与冲突规则

- 所有区间为 `[start, end)`；首尾相接不相交。
- 同轨视觉片段重叠计为冲突；音频轨允许重叠，只统计并发数和峰值。
- 零长、负长、越界分别归类为输入非法。
- 视觉轨按 `(z_order, track_id)` 决定上下层；遮蔽进入帧调度表，不计冲突。
- 锁定片段及其所在重叠簇不移动，相关冲突写入 `unresolved` 并附原因。

## 消解策略

- `trim`：截断后入片段的头部，并同步推进源入点。
- `ripple`：保持时长，将后续片段平移到前一活动片段之后。
- `spill`：把仍有冲突的片段移入独立的新轨。
- `drop`：在重叠对中丢弃较低优先级片段，同优先级丢弃较后者。
- `fill`：截断冲突后，用低优先级 filler 片段补齐视觉轨空隙。

策略逗号组合，默认按给定顺序尝试。`--scope track` 对整条轨执行，
默认 `--scope cluster` 对连通重叠簇逐组执行。

## ETJ 交换格式

ETJ v1 的时间字段为整毫秒：`start_ms`、`duration_ms`、
`source_in_ms`。v1 使用顶层 `tracks` 配置与扁平 `clips`。

ETJ v2 使用 `timebase: 48000`、轨道 `defaults` 继承、
`origin` 与片段 `offset` 相对定位。片段可写 `start` 代替 `offset`，
二者互斥。所有时间映射可写为 `{"samples": N}`，也可写秒、毫秒或帧。

v2 降级到 v1 时，每个未落在整毫秒格点的片段时间逐条产生 warning，
并采用银行家舍入。未知扩展字段通过 `extensions` 或平铺扩展键往返保留。

## OTIO 约束

时间以 48 kHz `RationalTime` 编码。重叠片段会分配到同一原轨的确定性
`lane` 序列，导入时按 `tlcore` metadata 中的原轨 ID 重新合并，因此
音频重叠和绝对位置都可保真。时间线 metadata、轨道 metadata、片段
metadata、优先级、锁定和扩展字段均写入 `tlcore` metadata 命名空间。

若轨道 metadata 中的 fps 与时间线 fps 不一致，会拒绝导入并报出轨道 ID。
`otio --export` 会立即重新导入并比较规范摘要，只有摘要一致才返回 0。

## 退出码

| code | 含义 |
| --- | --- |
| 0 | 成功；消解完成且精度无损 |
| 2 | 输入、模型、时间基或 OTIO 帧率不一致 |
| 3 | 仍有锁定等不可消解冲突 |
| 4 | 精度损失或时长 drift 超过阈值 |
| 5 | 文件读写或 OTIO 互转失败 |

`resolve` 与 `schedule` 可通过 `--drift-threshold-samples` 调整时长偏差阈值。

## 测试

```bash
python -m pytest -q
```

测试覆盖 banker's rounding、半开区间、5 万片段扫描预算、五种策略与
幂等性、锁定退出码、两版格式、OTIO 摘要往返、逐帧确定性和 CLI 验收流。
