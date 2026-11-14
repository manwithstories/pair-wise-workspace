# ABR 离线回放模拟器

本地生成带状态迁移的网络轨迹，逐段选择码率，用数值指标评估自适应码率策略。
纯终端形态，仅依赖 NumPy，无外部服务。

## 快速开始

```bash
python -m abr gen-trace --out traces/mixed.csv --seed 7 --duration 600 --regimes wifi,4g,3g --outages 3
python -m abr simulate --trace traces/mixed.csv --estimator dual --ladder ladders/main.json --chunk-sec 4 --buffer-cap 30 --print-json
python -m abr sweep --traces traces --estimators harmonic,ewma,dual --out out/sweep.csv
python -m abr report --input out/sweep.csv --top 10
python scripts/gen_traces.py   # 批量生成多场景样例轨迹
```

## 模块

- `abr/trace.py` 马尔可夫状态迁移逐秒轨迹，突发丢包与短时中断，CSV 逐行校验（错误带行号）
- `abr/estimator.py` 统一估计接口：滑动窗口调和平均 / 快慢双 EWMA / 双模鲁棒
- `abr/ladder.py` 码率阶梯 JSON 加载与单调性校验、分片字节换算
- `abr/buffer.py` 缓冲账本：水位、满缓冲暂停、耗尽事件与毫秒（启动前等待不计耗尽）
- `abr/decider.py` 升档滞后 + 紧急降档 + 换档冷却 + 启动阶段，阈值可注入
- `abr/simulator.py` 事件循环：分数秒下载结算，0 带宽长段推进时钟不死循环
- `abr/metrics.py` 切换次数、振幅、平均码率、启动时延、QoE（码率/1000 - 4.3*耗尽秒 - 1.0*切换）
- `abr/report.py` 终端表格与 JSON 输出

## 退出码

- 0 成功；2 参数错误或未知估计器；3 轨迹校验失败；4 无可用分片（轨迹短于一个分片）

## 测试

```bash
python -m unittest discover -s tests
```
