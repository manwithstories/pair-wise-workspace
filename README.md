# relay-timeline

区域直播分发的可恢复离线时间线重建：跨节点传输后修复包流、重建节目
时间线，离屏导出可复核的帧序列，并用码率、画质统计与时间对齐误差
验证结果。纯终端运行，无外部服务依赖。

## 环境准备（macOS）

```sh
brew install ffmpeg
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

## 验收命令

```sh
.venv/bin/python -m relay_timeline.cli demo --seed 41 --out /tmp/relay-demo
.venv/bin/python -m relay_timeline.cli verify --manifest /tmp/relay-demo/manifest.json
.venv/bin/python -m relay_timeline.cli demo --case truncated-output --out /tmp/relay-bad  # 预期退出码 3
```

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 验收通过 |
| 2  | 输入或清单无效 |
| 3  | 媒体解码或检查点损坏 |
| 4  | 验收指标超限 |

## 场景

`normal`、`duplicate-reorder`、`burst-loss`、`bandwidth-drop`、`mixed`
（默认，含中断恢复）、`invalid-cut`（退出码 2）、`truncated-output`
（退出码 3）、`interrupted-resume`。全场景核对：

```sh
.venv/bin/python scripts/demo.py
.venv/bin/python -m pytest
```

## 模块

- `timebase.py` 共享整数刻度与舍入规则
- `packet_repair.py` 去重、重排、缺失区间与突发丢失分类
- `timeline.py` 多轨合并、剪辑点校验、定长转场
- `render.py` PyAV 离屏编解码与帧序列导出
- `audio.py` 音轨偏移与峰值/RMS/静音统计
- `checkpoint.py` 分块原子提交与断点恢复
- `manifest.py` 对齐清单与码率摘要
- `pipeline.py` 端到端编排；`simulate.py` 本地故障注入
