# mediasync

离线媒体预处理内核：把 H.264 / H.265 / AV1 裸码流与关键帧索引处理成适合
移动网络低延迟互动回放的分片清单。纯终端、纯本地、无外部服务依赖。

## 能力

- **裸码流扫描**：解析三类编码的 Annex B 起始码与 NAL/OBU 类型，报告字节偏移、
  时间戳与错误类型；关键帧索引拒绝越界、倒序、重复及未指向关键帧的项。
- **共享时钟**：用有理数（`Fraction`）统一视频帧、音频样本与索引时间；
  时间戳回退或编码器重启时分段（epoch）记录，不静默改写源时间。
- **共同切点**：按多表示层可用关键帧求共同切点，超出最大偏差阈值则拒绝并
  输出未对齐原因。
- **音频归一**：统一采样率与 mono/stereo/5.1 布局，缺声道补零、额外声道
  drop/fold、不足一帧尾部 pad/truncate，全部留痕。
- **色彩裁决**：检查 range/matrix/transfer/primaries，缺失按显式策略补齐，
  冲突保留各来源原值并按固定优先级裁决。
- **可恢复检查点**：固定块提交，临时记录校验后原子替换；恢复时校验输入摘要、
  配置摘要与块校验和，不匹配则隔离旧检查点并从安全边界重算。

## 安装

```bash
brew install ffmpeg libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install -e ".[test]"
```

## 测试

```bash
.venv/bin/python -m pytest -q
```

## 演示

```bash
.venv/bin/python -m mediasync.cli demo \
  --work ./demo-out \
  --corrupt-every 7 \
  --interrupt-after 3 \
  --resume \
  --representations 3 \
  --segment-ms 500
```

打印的可核对字段：

| 字段 | 含义 |
| --- | --- |
| `corrupt_offset_bytes` | 扫描器定位的损坏字节偏移 |
| `corrupt_locate_error_bytes` | 与注入位置的误差（要求 ≤ 1） |
| `resume_ms` | 续跑耗时（1000ms 素材目标 < 500ms） |
| `segments` | 分片数量（不少于 2） |
| `aligned_representations` | 对齐表示层，如 `3/3` |
| `manifest_sha256` / `index_sha256` | 清单与索引摘要（64 位十六进制） |

重复执行（相同参数、不同工作目录）摘要保持一致。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | 参数错误 |
| 3 | 输入损坏或无法对齐 |
| 4 | 检查点校验失败 |

## 模块结构

```
src/mediasync/
  clock.py       有理数时间基准与 epoch 分段
  errors.py      可序列化错误模型与致命分类
  bitstream.py   三类裸码流扫描与关键帧索引校验
  audio.py       采样率/声道布局归一
  color.py       色彩元数据检查与裁决
  segment.py     共同切点与分片清单
  checkpoint.py  分块检查点、原子替换与隔离
  recovery.py    恢复协调（摘要/校验和验证）
  fixture.py     PyAV 真实编码与损坏注入原语
  workspace.py   演示工作区物料生成
  pipeline.py    流水线编排、取消与恢复
  cli.py         终端命令适配器
```

## 安全约束

所有素材与生成文件只读写 `--work` 指定目录，`..` 越界与绝对路径逃逸被拒绝；
不联网、不引用外部文件，夹具全部由代码确定性生成。
