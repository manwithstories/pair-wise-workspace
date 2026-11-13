# tlrender

独立动画工作室字幕后期组使用的离线时间线消解与首帧渲染工具。全程无图形界面、
无外部服务，Pillow 只写入内存缓冲，NumPy 负责 alpha 合成。

## 安装（一条命令）

```bash
python -m pip install -r requirements.txt
```

## 时间线格式

- **v1（legacy）**：毫秒整数，`cue` 嵌在 `shot` 节点内。
- **v2（现行）**：扁平 `track`，帧号 + 有理数 `fps`（如 `24000/1001`）。

结构错误整份拒绝（`ParseError`，含文件名与行号）；单条语义错误（零时长、
负时间、超过 10 分钟）记 warning 并跳过该条。

## 生成夹具与期望值

```bash
python -m tlrender.cli gen-fixtures --dir fixtures
```

产出 `fixtures/timeline_v1.tl`、`fixtures/timeline_v2.tl`、`fixtures/expected.json`。

## 验收演示

```bash
python -m tlrender.cli run \
  --timeline fixtures/timeline_v2.tl \
  --legacy fixtures/timeline_v1.tl \
  --out outputs --json
```

每行一条 JSON 记录，末行汇总：

```text
total 42, resolved 17, dropped 1, frames 42, hash_mismatch 0
```

## 模块与数据流

`cli` → `tl_parse`（双格式解析 + 诊断）→ `timebase`（有理数帧率换算）→
`overlap`（同轨半开区间消解）→ `transition`（cut/dissolve/wipe 避让）→
`compose`（Pillow + NumPy 首帧）→ `digest`（SHA-256 + manifest）→
`report`（终端表 / JSON）。

### 关键规则

- 区间为半开 `[start, end)`，端点相接不算重叠。
- 稳定排序键：`(track, start, priority 降序, id)`；高优先级完整保留，低优先级截断，
  剩余不足 400ms 标记 `drop`。
- 帧→毫秒按半值向上取整，单点误差 < 0.5ms，累计不超过 1ms。
- 转场窗口不可跨越；压到 400ms 以下时整体平移，取移动距离更短的一侧。
- 每条 cue 落一张首帧 PNG，SHA-256 取 RGB 原始字节摘要前 16 位写入 `manifest.json`。
- `--no-timestamp` 时同输入逐字节可复现；重跑会比对旧 manifest 并报告 `hash_mismatch`。

## 退出码

| 场景 | 退出码 |
| --- | --- |
| 成功 | 0 |
| 文件缺失 / 非 UTF-8 / 结构错误 / 超 20MB | 1 |
| 单条字幕超过 10 分钟（产物仍落盘） | 2 |

## 测试

```bash
python -m pytest tests -q
```
