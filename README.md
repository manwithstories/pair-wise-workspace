# 双声道时间线混音器

把多供应商、多采样率/声道数/偏移的音频素材按时间线混成双声道母带,
在对白、音效与音乐段落之间用自动化曲线调整增益。纯本地、无服务依赖,
离线与流式两条执行路径**逐采样位级一致**。

## 技术栈

Python 3.11+、NumPy 1.26、soundfile 0.12;标准库 argparse/json/pathlib/
threading/unittest,无独立服务进程。

## 快速演示

```bash
python cli.py generate --out .demo
python cli.py mix --config .demo/project.json \
    --output .demo/offline.wav --report .demo/offline.json --mode offline
python cli.py mix --config .demo/project.json \
    --output .demo/stream.wav --report .demo/stream.json \
    --mode stream --block-frames 1024
python cli.py verify --left .demo/offline.wav --right .demo/stream.wav \
    --report .demo/verify.json
```

验收要点:`sample_rate=48000`、`channels=2`、`frames≈240000`、
`tracks=4`、`automation_samples=20`,`verify.max_abs_diff=0.0`(容差 1e-6)。

## 退出码契约

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 2 | 参数或配置错误(缺字段、非法取值、坏 JSON) |
| 3 | 输入媒体错误(空文件、损坏 WAV、非正采样率、文件缺失) |
| 4 | 输出写入失败(目录不可写、写中断) |
| 5 | 离线与流式数值不一致(超 `--tolerance`,默认 1e-6) |

## 模块架构

```
types.py       数据类、错误层级(退出码对齐)
config.py      project.json 解析与字段校验
resample.py    加 Hann 窗 sinc 确定性重采样,分块与整段位级一致
automation.py  分段线性曲线:乱序排序、重复点拒绝、首尾保持、NaN/Inf 拒绝
graph.py       声道映射(mono 复制 / stereo 交换 / 多声道显式 downmix)、增益、求和
sources.py     FileSource 惰性读取、MemorySource、PrefetchSource 有界预取线程
stream.py      BlockEngine:两条路径共享的固定块混音核心
metrics.py     峰值、RMS、有效 RMS、削波计数(主总线 + 轨道混音前)
offline.py     源工厂、fail-fast/best-effort 策略、WAV/JSON 落盘
fixtures.py    确定性夹具与样例工程(44.1k/48k/96k、mono/stereo/3ch、故障件)
cli.py         generate / mix / verify 子命令
tests/         48 个 unittest 用例
```

## 关键设计

- **离线/流式一致性**:两者共用 `BlockEngine`,同一套 float64 块内运算顺序;
  重采样器是纯函数式加窗 sinc,任何分块序列产出相同样本,因此输出 WAV
  `max_abs_diff=0.0`,而不仅是容差内接近。
- **内存约束**:流式路径源按块惰性读取,`PrefetchSource` 用有界队列
  (容量 4)的 daemon 线程预取,队列满即阻塞,内存上界与轨道总长无关。
- **禁止静默丢声道**:mono 自动复制双声道;多声道(>2)必须给 `routes`,
  为每个源声道显式指定 `L`/`R`/`L+R`,缺一个即配置错误。
- **削波统计**:混音前每轨 `track_stats[].clipped_samples` 与混音后主总线
  `clipped_samples` 同时报告,阈值 `clip_threshold` 可配置。
- **best-effort**:坏轨跳过并在 `skipped_tracks` 记录 `error`/`reason`,
  不伪造成功;fail-fast 下任一坏轨立即以退出码 3 终止。
- **可复现**:全部浮点比较、重采样窗宽、削波阈值均为显式参数;夹具由
  固定相位正弦合成,无随机种子依赖。

## 测试

```bash
python -m unittest discover -s tests -v
```
