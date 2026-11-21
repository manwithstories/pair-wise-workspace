# mediapipe — 流式、可续跑、无损的 MP4/MOV 重封装流水线

面向移动网络（地铁/电梯/热点切换）低延迟互动音视频场景，把录制端产生的
带时间戳 MP4/MOV 素材**流式解复用 → 轨道映射 → 无损转封装 → 回读校验**，
支持慢消费者背压、损坏定位与包边界检查点续跑。全程不解码、不重编码
压缩音视频包。

## 环境与安装（纯本地，无网络服务依赖）

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install "av==12.*" "numpy==2.*" "soundfile==0.12.*"
.venv/bin/python -m pip install -e .
```

## 生成夹具与验收

```bash
.venv/bin/python scripts/demo.py --generate-fixtures --dir .local-fixtures
.venv/bin/python -m mediapipe.cli remux \
    --input .local-fixtures/sample.mov --output /tmp/remux.mov \
    --checkpoint /tmp/remux.ckpt --queue-bytes 1048576 \
    --slow-consumer-ms 5 --verify
```

损坏样例（应返回退出码 3，并打印阶段/流号/偏移/损坏类型）：

```bash
.venv/bin/python -m mediapipe.cli remux \
    --input .local-fixtures/truncated.mp4 --output /tmp/bad.mp4 \
    --checkpoint /tmp/bad.ckpt --verify
```

中断后重复原命令即从最近检查点恢复，输出恢复包序号与 `resume_ms`。

## 退出码契约

| 码 | 含义 |
|----|------|
| 0 | 处理与回读校验通过 |
| 2 | 输入或轨道映射无效 |
| 3 | 可定位的媒体损坏（截断/坏 box/坏包/缺失索引） |
| 4 | 续跑状态与当前输入/映射不兼容（指纹不符） |
| 5 | 内部写出或回读验证失败 |

## 模块划分

| 模块 | 职责 |
|------|------|
| `timebase.py` | 统一整数微秒时间基准、PTS/DTS 换算与 int64 溢出检测 |
| `errors.py` | 携带阶段/流号/源偏移（精确点或半开区间）的错误模型 |
| `packet.py` | 不可变包记录、压缩负载引用、源字节区间、逐包 SHA-256 |
| `buffer.py` | 字节+包数双上限有界队列，BLOCK/ABORT 背压与峰值统计 |
| `demux.py` | 流式解复用、流报告、负时间戳/DTS 单调性校验 |
| `container_scan.py` | ISO-BMFF box 流式预检，定位越界 box 与缺失索引 |
| `mapping.py` | 写出前轨道映射校验（缺失/重复/类型不符） |
| `remux.py` | fragmented MP4/MOV 无损重封装，负载与时间戳原样透传 |
| `checkpoint.py` | 文件/映射指纹、原子替换检查点、路径安全、续跑兼容校验 |
| `validate.py` | 回读逐包 SHA-256、包数与微秒域时间线比对 |
| `pipeline.py` | 生产者-消费者编排、检查点续跑、故障停止下游 |
| `fixtures.py` | 样例生成与四类损坏注入（返回真实字节偏移） |
| `cli.py` | 终端 JSON 行输出与退出码映射 |

## 关键设计与边界

- **无损**：输出采用 fragmented MP4（`+frag_keyframe+empty_moov`），压缩
  字节逐包 SHA-256 比对，非文件大小比较；校验和由输入与输出压缩字节
  分别逐包计算。
- **容器级差异显式报告**：碎片化封装会折叠源 edit-list/priming，造成统一
  等幅时间线平移与前导包 duration 清零，记入 `metadata_not_preserved`，
  不静默处理；非线性时间戳偏差仍判失败。
- **内存有界**：队列同时受字节与包数上限约束，内存占用不随输入时长增长。
- **偏移定位不臆造**：能精确到字节给精确点，只能界定范围给半开区间
  （`exact=false`），绝不伪称精确定位。
- **检查点**：临时文件 + fsync + `os.replace` 原子替换；拒绝目录、符号
  链接逃逸与覆盖输入；输入或映射指纹变化一律拒绝续跑。
- **不承诺修复**：不可解码或缺失关键索引的内容保留诊断信息并以码 3 失败。

## 测试

```bash
.venv/bin/python -m unittest tests.test_pipeline -v
```
