# mediarail

区域电视台素材整备：流式修复 MP4/MOV 节目时间轴并重建可随机访问索引。

## 安装

```sh
brew install ffmpeg
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

## 用法

```sh
# 生成验收样例（正常 / VFR 边界 / 截断损坏）
.venv/bin/python -m tests.make_fixtures --out .tmp/fixtures

# 批量整备：流式扫描 → 时间戳修复 → 包日志 → 封装 → 索引 → 验证
.venv/bin/python -m mediarail.cli run \
    --input .tmp/fixtures --output .tmp/out --checkpoint .tmp/state \
    --queue-size 8 --report-json

# 中断续跑：校验输入指纹，临时产物截断到最近提交边界后继续
.venv/bin/python -m mediarail.cli resume \
    --input .tmp/fixtures/boundary.mov --output .tmp/out/boundary.mp4 \
    --checkpoint .tmp/state/boundary

# 归档核对：逐条解码验证索引并输出 SHA-256
.venv/bin/python -m mediarail.cli verify \
    --input .tmp/out/boundary.mp4 --index .tmp/out/boundary.midx --sha256
```

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 完成且索引验证通过 |
| 2 | 存在不可自动修复素材（保留证据并定位首个异常包） |
| 3 | 输入变化或检查点不可信，拒绝续跑 |
| 4 | 写入或校验失败 |

## 架构

```
probe → repair → journal（临时产物）→ mux → index → verify
  └ checkpoint：输入指纹 + 阶段游标，包边界提交
```

- `timebase`：有理数时间基准、在线漂移估计（>200 ppm 分段）、回绕展开
  （区分真实回绕 / 乱序 / 重置 / 不可判定）。
- `probe`：PyAV 流式扫包，不整片载入内存；截断与损坏包按包序号和
  字节偏移定位。
- `repair`：共享时间基准校正时间戳；VFR 轨道按包序重建呈现顺序；
  重复时间戳、负持续时间、音画偏差 >40 ms 生成定位记录。
- `pipeline`：有界队列连接各阶段，记录背压时长与峰值，取消状态全程
  传播；全局内存预算限制并发文件的在途包总量。
- `checkpoint`：输入大小 / mtime / 分块 SHA-256 指纹；输入变化拒绝续跑。
- `index`：关键帧 + 音频同步点随机访问索引（`.midx`），逐条解码验证，
  验证失败拒绝发布。

## 测试

```sh
.venv/bin/python -m pytest tests/ -q
```
