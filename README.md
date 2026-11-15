# ABRL — 纯 Python 自适应码率参数扫描与阶梯生成工具

面向短视频托管/点播分发的命令行工具：把编码参数网格展开为可去重、可断点续跑的
任务，真实写出码流并回算实测码率/PSNR/SSIM，落 SQLite 后在单调性与码率比约束下
生成可直接下发的 ABR 阶梯。无外部编码器进程，仅依赖 Python 3.11+、NumPy 与标准库。

## 模块结构

- `src/abrl/media/yuv.py`：YUV420p 平面读写、帧迭代、分辨率缩放、源文件摘要
- `src/abrl/codec/dct.py`：8x8 整数 DCT/IDCT、量化表、zigzag 扫描
- `src/abrl/codec/encoder.py`：I/P 帧、GOP/QP 控制、RLE+zlib 码流封装与回读解码
- `src/abrl/metrics/quality.py`：PSNR（相同帧 99.0 哨兵）、8x8 窗 SSIM、逐帧聚合
- `src/abrl/scheduler/grid.py`：网格展开、任务键哈希、去重与同键异参冲突
- `src/abrl/scheduler/runner.py`：有界线程池、SIGINT/SIGTERM、两次重试、孤儿回收
- `src/abrl/store/db.py`：表结构、WAL、原子领取任务、断点/孤儿查询
- `src/abrl/ladder/builder.py`：约束搜索、单调性校验、0.4dB 冗余合并、不可行诊断
- `src/abrl/report/render.py`：终端表格与 JSON 报告
- `src/abrl/cli.py`：`fixture/sweep/ladder/verify/report` 子命令与统一退出码

## 快速演示

```bash
python scripts/gen_fixture.py --out fixtures/a.yuv --frames 96 --size 640x360 --seed 7
python -m abrl.cli sweep --source fixtures/a.yuv --grid configs/grid.json --db var/s.db --workers 2
python -m abrl.cli ladder --db var/s.db --rungs 4 --max-kbps 2400 \
    --min-ratio 1.30 --max-ratio 2.20 --out var/ladder.json
python -m abrl.cli verify --db var/s.db --ladder var/ladder.json --json
```

扫描中按 Ctrl-C（或 kill -9）后重跑同一命令：已完成任务跳过，遗留 `running`
在启动时标记为 orphaned 重新入队，并打印 `resumed` 与 `orphaned_reclaimed` 计数。

## 任务键与去重

任务键 = SHA-256(源文件摘要, 宽高, 帧率, QP, GOP, 编码器版本) 前 16 位。
同键只保留一条；同键异参直接判为输入错误（退出码 4）。

## 码率与质量

码流先写 `*.tmp` 再原子改名，崩溃不留半截文件。实测码率恒等于
`字节数 * 8 / 时长`，质量在解码回帧后计算，不做估算。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 / 校验通过 |
| 2 | 阶梯不可行 |
| 3 | 库中存在 failed 任务 |
| 4 | 输入或库结构错误 |
| 130 | 扫描被信号中断（已落盘的进度可续跑） |

## 测试

```bash
cd tests && python -m unittest discover
```
