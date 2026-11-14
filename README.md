# abrlab

面向司法鉴定远程问询录像归档的本地 ABR 阶梯实验工具。所有片源、压缩流与指标均由代码生成和复算，不读取外部视频，不调用外部编解码器，也不访问网络。

## 安装

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

运行时只依赖 Python 3.11+、NumPy 1.26.4、`sqlite3` 和 `zlib`。

## 验收流程

```bash
.venv/bin/python -m abrlab plan --demo --inject-duplicates 3 --out work/plan.json
.venv/bin/python -m abrlab scan --plan work/plan.json --db work/scan.db --stop-after 10 --json
.venv/bin/python -m abrlab scan --resume --db work/scan.db --json
.venv/bin/python -m abrlab ladder --db work/scan.db --out out/ladder.json \
  --max-rungs 4 --max-bitrate 1800 --min-quality 0.86 --json
.venv/bin/python -m abrlab verify --db work/scan.db --ladder out/ladder.json
```

预期行为：

- 演示计划含 51 条原始记录，规范化去重后 48 个唯一任务。
- 首次扫描在完成 10 个任务后以退出码 4 中断；`resume` 从 SQLite 中的 pending/running 任务继续，并报告 `skipped=10`。
- 阶梯来自实测点，不套固定码率表；默认最多 4 档。
- `verify` 从数据库读取真实压缩流，重新解码、重建参考片源并复算码率、PSNR 和 SSIM，输出 `monotone` 与 `violations`。

## 模块

- `plan.py`：计划解析、边界校验、SHA-256 任务键、重复项规范化和演示计划生成。
- `source.py`：固定种子片源、基准分辨率画质参考和确定性缩放。
- `codec.py`：块 DCT、量化、zlib 熵编码、真实字节流解码。
- `metrics.py`：PSNR、SSIM、常量帧和极小尺寸的稳定处理。
- `store.py`：SQLite 表、事务、任务状态机、失联任务恢复和结果 BLOB。
- `scheduler.py`：单进程调度、重试隔离、断点续跑、临时文件清理和原子结果提交。
- `ladder.py`：实测点帕累托前沿、相邻档位合并、自适应选档和独立校验。
- `report.py`：终端表格及 JSON 汇总。
- `cli.py`：`plan`、`scan`、`ladder`、`verify` 子命令及退出码。

## 数据口径

每个任务记录源规格和编码参数。任务键是二者规范化 JSON 的 SHA-256；相同计划重复执行不会重算已完成任务。

编码器对 Y/U/V 平面做帧内块 DCT 和标量量化，再把量化系数用 zlib 压缩。码率按 `实际字节数 × 8 / 时长` 计算，时长由帧数和 fps 得出。

为了在同一质量口径比较不同分辨率，PSNR/SSIM 在解码帧归一化到 `base_width × base_height` 后，与同口径参考帧计算。SSIM 使用逐帧局部窗口，常量帧使用稳定值，PSNR 在完全相同时封顶为 99 dB。

## 状态与退出码

状态为 `pending → running → done|failed`。每次领取任务增加 `attempts`，并记录耗时和字节数。进程重启后，遗留的 `running` 会恢复为 `pending`；累积三次失败转 `failed` 并保留错误码。

退出码：

- `0`：成功且单调。
- `2`：计划或参数非法。
- `3`：阶梯不可行、校验失败或非单调。
- `4`：受控中断，可续跑。
- `5`：存在超过重试上限的任务。

## 边界与资源

计划会限制帧数、尺寸、块大小、QP、色度模式和单任务样本量，避免异常参数放大内存。默认演示网格的 48 档在当前机器约 8 秒完成，远低于 120 秒限制；单进程只保留当前任务的片源、压缩流和指标数组。`work/`、`out/`、SQLite 文件、虚拟环境和 `__pycache__` 均不纳入交付。

## 测试

```bash
.venv/bin/python -m unittest discover -s tests -v
```

测试夹具全部由代码在临时目录中生成。
