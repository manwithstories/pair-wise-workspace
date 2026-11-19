# relay-media

区域直播分发场景下的边缘媒体片段可靠转发工具链（纯本地、终端形态）。

## 能力

- 扫描 H.264 / H.265 / AV1 裸码流，记录每个 NAL/OBU 的原始字节偏移与截断/损坏状态。
- 校验、补建关键帧索引，处理缺页、偏移越界、非单调与重复项。
- 以整数微秒为唯一媒体时间基准，处理时间戳缺失、回绕、非单调与可变帧率。
- 按目标时长 / 关键帧 / 最大字节切片，输出片段清单与源偏移范围。
- PCM/WAV 静音检测（声道合并、短静音门限、尾截断），映射到统一时间轴。
- 音视频感知指纹，识别完全重复与汉明距离门限内的近似重复。
- 传输模拟器：突发丢包、乱序、重复投递、节点重连与恢复指标。
- 分阶段流水线，错误保留输入字节偏移与阶段名，支持阶段边界恢复与确定性重跑。

## 安装与验收

```bash
cd pair-flow
python3.12 -m venv .venv
.venv/bin/python -m pip install 'av==12.3.0' 'numpy==2.1.3' 'soundfile==0.12.1'
.venv/bin/python -m pip install -e .
.venv/bin/python -m relay_media.cli demo --codec h264 --duration 12 \
    --loss-burst 0.08 --truncate-at 4096 --output /tmp/relay-report.json
.venv/bin/python -m relay_media.cli verify /tmp/relay-report.json
```

## 退出码

| 码值 | 含义 |
| --- | --- |
| 0 | 验证通过 |
| 2 | 输入或参数错误 |
| 3 | 存在不可恢复损坏 |
| 4 | 指标未达标 |

仅处理自生成或用户指定的本地素材，全程不联网。
