# mediabridge

流式修复、可续跑索引与随机跳转校验，面向跨节点传输中损坏的 MP4/MOV 直播归档。

> 纯本地终端工具。无图形界面、无浏览器、无摄像头/麦克风权限、无外部服务依赖。

---

## 1. 它解决什么问题

区域直播分发团队在机房与边缘节点之间搬运归档文件时，三类损伤最常见：

| 损伤 | 表现 | 本工具的处理 |
| --- | --- | --- |
| 编码器时钟跳变 | dts/pts 重复、倒退、跳变数秒 | 按容差重映射到统一时间线，并**精确报告**轨道、包序号、原始时间戳 |
| 磁盘写入中断 | 文件尾部截断 | 保留可读前缀，记录截断点，退出码 0 |
| 容器索引损坏 | `moov` 缺失、偏移指到文件外 | 明确判定为结构错误，退出码 2，不做假修复 |

同时解决三个工程问题：

* **内存有界**：解码包走有界队列，峰值常驻内存远低于 256 MiB，慢消费者以阻塞生产端的方式形成背压。
* **精确定位**：`first_error_packet` 与生成器注入位置误差不超过 1 包。
* **可证明的续跑**：中断后再跑，输出与一次跑完**逐字节相同**（SHA-256 相等）。

---

## 2. 安装

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install 'av==12.3.0' 'numpy==2.1.3' 'soundfile==0.12.1'
```

第三方依赖只装在项目根目录的 `.venv` 中；不要对系统 Python 执行 `pip install`。
项目本身以源码布局提供（`src/mediabridge`），根目录的同名包只是把
`python -m mediabridge` 的导入路径指到 `src`，无需 `pip install -e .`。

---

## 3. 快速体验（验收命令）

```bash
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m mediabridge sample --out .work/samples

.venv/bin/python -m mediabridge repair .work/samples/corrupt.mov \
    --index .work/index.sqlite --checkpoint .work/checkpoint.json \
    --queue-size 8 --chunk-packets 128 --interrupt-after 512

.venv/bin/python -m mediabridge repair .work/samples/corrupt.mov \
    --index .work/index.sqlite --checkpoint .work/checkpoint.json \
    --queue-size 8 --chunk-packets 128 --resume

.venv/bin/python -m mediabridge verify .work/samples/corrupt.mov \
    --index .work/index.sqlite --report-json
```

每条命令都向 stdout 打印一行 JSON（便于批处理），向 stderr 打印一行人类可读摘要。

### 输出字段

| 字段 | 含义 |
| --- | --- |
| `first_error_packet` | 首个异常包的全局序号；与生成器注入位置误差 ≤ 1 包 |
| `resume_start_packet` | 续跑时从哪个全局包之后继续 |
| `resume_seconds` | 已提交边界的媒体时间（秒） |
| `peak_queue_depth` | 本次运行的峰值队列深度；不超过 `--queue-size` |
| `index_rows` | 索引中可跳转的行数（至少覆盖所有视频关键帧） |
| `output_sha256` | 修复文件 SHA-256（64 位十六进制）；完整运行与续跑必须相同 |

### 退出码

| 码 | 含义 |
| --- | --- |
| `0` | 修复/校验通过（损伤被跳过并记录） |
| `2` | 输入损坏且不可恢复（无流、编解码器不支持、`moov` 缺失等） |
| `3` | 断点或索引与当前输入/运行不匹配（源已变化、版本不兼容、索引损坏） |
| `4` | 内部处理失败 |

---

## 4. 模块结构

```
src/mediabridge/
  cli.py         argparse 子命令与 JSON 摘要输出
  errors.py      可定位、可恢复的错误模型（含退出码映射）
  timeline.py    统一时间基准、修复规则、可变帧率与采样率换算
  probe.py       流式探测容器、轨道与包（不整文件读入）
  mp4write.py    分片 MP4 写入与 tfdt/时长补丁（随机跳转的基础）
  pipeline.py    有界队列驱动的确定性分块规划器（repair 与 verify 共用）
  index.py       SQLite 随机访问索引，按块事务提交 + 校验和
  checkpoint.py  批次游标、输入指纹与提交边界的原子持久化
  repair.py      生产/消费、断点续跑、时长补丁与摘要
  verify.py      跳转抽检、偏移/哈希校验、逐块校验和比对
  samplegen.py   由代码生成正常/截断/损坏/跳变/零包/慢消费者/不支持编码素材
tests/test_pipeline.py
```

数据流：

```
samplegen → probe → timeline → repair → index
                        ↑                    ↓
                   checkpoint            verify
```

`repair`、`index`、`verify` 共用 `timeline` 与 `errors` 的定义。

---

## 5. 关键设计

### 5.1 为什么输出是分片 MP4（fMP4）

修复文件必须同时满足三件事：

1. 普通播放器可解码（H.264 + AAC）；
2. **任意关键帧可随机跳转**；
3. 续跑结果与完整运行逐字节一致。

普通 MOV 的跳转表（`stss`/`stco`）只在文件尾的 `moov` 中，增量写出的半成品根本无法打开。
分片 MP4 把 `ftyp+moov` 作为初始化段，之后每块一个 `moof+mdat`：

* 每提交一块，索引立即可用；
* 追加或从某块之后重建都不必回写前面的字节；
* `omit_tfhd_offset` 让样本偏移相对自身 `moof`，落点无关。

> 一个容易踩的坑：`av.Packet(p)` 复制会**丢失 `is_keyframe` 标志**，
> 导致 muxer 无法把分片首个样本标为同步样本，libavformat 于是拒绝向后跳转。
> 因此 `pipeline` 在**原地**改写包的 dts/pts（决策已由 `PacketInfo` 快照算出，重构安全）。

### 5.2 确定性分块

块在「已有 `chunk-packets` 个包且下一个包是视频关键帧」时闭合。该规则只依赖包序列本身，
所以中断点与完整运行的块边界逐一对应 —— 这是续跑能逐字节复现的根本原因。

### 5.3 提交顺序

每块：写输出 → `fsync` → 一个 SQLite 事务提交该块的索引行 → 原子替换检查点。
任一步崩溃，最多留下未提交的尾部字节，续跑时先截断回最后一个已提交长度。
索引与检查点都经由临时文件 + `os.replace` 落盘，绝不暴露半成品状态。

### 5.4 损伤判定

* 音频：**每包一个全新 AAC 解码器**，检测无状态，两次运行结论必然一致。
* 视频：每个块内复用一个上下文，块边界总在关键帧处重置 —— 续跑从已提交边界开始时，解码器状态与完整运行完全相同。
* 视频解码失败后丢弃后续非关键帧直到下一个关键帧（避免参考丢失包产生花屏），并记录为损坏区间。
  音频损坏只丢该包。

### 5.5 时间线修复

* 首包锚定原点，负时间戳夹到 0（AAC priming 的 -1024 是典型情况）。
* 32 位回绕按 `±2^32` 展开。
* 重复/倒退按标称间隔（视频帧率或 AAC 1024/采样率）前推预测。
* 超过容差的**前后跳变**不当作致命错误：整轨重定基（rebase）到上一输出后一个间隔，
  并在 `timestamp_repairs` 中带原始时间戳与偏差报告。
* 只有「连续异常多到无法分辨真伪」才抛出 `TimestampToleranceError`。

---

## 6. 夹具（`sample` 子命令）

全部由 `samplegen.py` 用固定随机种子与 `bitexact` 生成，两次运行逐字节相同：

| 文件 | 内容 | 期望 |
| --- | --- | --- |
| `normal.mov` | 干净 H.264+AAC，约 923 包 | 修复成功 |
| `truncated.mov` | faststart MOV，`mdat` 尾部被切 | 修复成功，记录截断 |
| `broken_index.mov` | 删除尾部 `moov` | 退出码 2 |
| `corrupt.mov` | 一个视频包 + 一个音频包被清零 | 修复成功，`first_error_packet` 精准 |
| `jitter.mp4` | 分片文件重写 `tfdt` 制造跳变/重复 | 修复成功，跳变被报告 |
| `zero_packets.mp4` | 只有初始化段、无样本 | 退出码 2 |
| `slow_consumer.mov` | 更长素材 | 配合 `--consumer-delay` 观察背压 |
| `unsupported_codec.mov` | MPEG-4 part 2 | 退出码 2，提示不支持 |

每个夹具附 `<name>.<ext>.json` 附带真相（注入位置等），测试据此断言而不是猜测。

---

## 7. 断点与索引的拒绝条件

续跑前核对：输入大小、修改时间、头/尾摘要、修复配置、工具版本、索引版本；
任一不符即退出码 3 并说明原因。索引损坏、版本不符、偏移越界、逐块校验和不符同样退出码 3。
「输入变化」「检查点不兼容」「索引不兼容」在 JSON 中分别有独立 `code`。

---

## 8. 约束对照

| 约束 | 实现 |
| --- | --- |
| 峰值常驻内存 ≤ 256 MiB | 有界队列 + 分块；测试用子进程 `ru_maxrss` 断言 |
| 支持 H.264/AAC、常见 MOV/MP4 时间基 | 见 `probe.SUPPORTED_*`；其他编码明确报错 |
| 仅读写用户指定本地文件，不覆盖源素材 | 同路径/硬链接直接拒绝；测试覆盖 |
| 临时文件 + 原子替换 | 索引 `.partial` → `os.replace`；检查点同理 |
| 拒绝路径逃逸 | 输出与源相同或指向源硬链接时拒绝 |
| `.venv` 置于根目录 | 所有验收命令用 `.venv/bin/python` |
