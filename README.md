# media_cli

本地纪录片/品牌短片素材检查工具。解封装、GOP 显示序重排和时间戳修复只有一份实现，`timeline` 与 `stats` 共用同一套帧序号和时间轴。

## 环境

- Python 3.11+
- PyAV 12.3
- NumPy 1.26
- pytest 8

仅使用本地文件和上述依赖，不访问网络，不打开预览窗口。

## 快速开始

```bash
python tests/fixtures/gen.py --out fixtures --cases 5
python -m media_cli scan --input fixtures/bad_pts.mp4 --json
python -m media_cli stats --input fixtures/bad_pts.mp4 --json --out out/stats.json
python -m media_cli timeline --input fixtures/bad_pts.mp4 --json
python -m media_cli verify --input fixtures/bad_pts.mp4 --json
pytest -q
```

`stats --out out/stats.csv` 或 `timeline --out out/timeline.csv` 会按扩展名输出逐帧 CSV。其他命令输出 JSON。

## 退出码

- `0`：成功
- `1`：`verify` 发现两条路径不一致
- `2`：参数错误、文件不可读或无视频流
- `3`：尾部截断，已输出最后一个完整帧的结果
- `4`：PTS 与 DTS 均缺失，时间戳无法修复

## 数据流

`container` 探测流参数，`packets` 抽取解码序记录，`reorder` 在 GOP 内按 PTS 恢复显示序，`timestamps` 统一处理 32 位回绕、缺失、重复和首帧偏移。`frame_source` 产出唯一的 `FrameRecord` 流；`pixel_stats` 只流式保留当前帧像素，`frame_kinds` 与 `timeline` 复用同一时间轴。

夹具与 `out/` 报告为生成物，不纳入版本控制；`tests/fixtures/gen.py` 会全部本地合成。
