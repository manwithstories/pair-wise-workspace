# media-deliver

现场多机位素材的时钟修复与剪辑时间线交付流水线（纯本地、无外部服务依赖）。

## 构建

```bash
brew install ffmpeg catch2 cmake ninja
cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=Release
cmake --build build
ctest --test-dir build --output-on-failure
```

## 一键验收

```bash
./scripts/demo.sh
```

或手动执行：

```bash
./build/media-deliver demo --seconds 12 --drift-ppm 180 --wrap-pts --vfr \
  --corrupt-every 97 --queue-packets 64 --target-bitrate 1800000 --json report.json
# 截断（致命故障，退出码 2 并报告首个坏包）
./build/media-deliver demo --truncate-at 43 --queue-packets 8
```

退出码：`0` 成功，`1` 参数错误，`2` 处理失败（致命故障）。

## 模块

| 文件 | 职责 |
| --- | --- |
| `time_model` | 统一微秒时间基、33 位 PTS 常量、漂移参数、错误分类、线性拟合 |
| `packet` | 包与剪辑/转场边界状态 |
| `manifest` | TRACK/EDIT/XFADE 清单解析与剪辑点最近帧对齐 |
| `clock_repair` | 33 位回绕展开、倒退歧义识别、采样/帧锚点漂移估计与斜率钳制 |
| `stream_pipeline` | 有界队列背压、下游致命错误取消并释放生产者 |
| `demuxer` | 侧车真值解析、截断与零时长判定 |
| `index_builder` | 关键帧随机访问索引、双向最近邻定位、tmp+rename+done 原子写入 |
| `decoder` | MJPEG 解码与逐帧 PSNR 度量 |
| `generator`/`gen_patterns`/`ffmpeg_util` | 确定性合成两路视频+音轨，注入漂移/回绕/VFR/坏包，码率校准 |
| `quality_report` | 码率偏差、PSNR、JSONL 诊断、JSON 报告 |
| `pipeline` | 全流程编排与跨轨指标聚合 |

## 关键设计

- **统一时间基**：所有轨道修复后映射到微秒有理数时间，并按首帧归零，消除设备绝对起点差异。
- **回绕判定**：在 33 位模空间计算前向增量，区分合法回绕与小幅非法倒退；无法安全修复时记录首个包位置并标记区段不可交付。
- **漂移估计**：侧车提供剥离 VFR 的线性化时间戳，与理想展示时间做线性拟合，得到真实晶振斜率并钳制；锚点不足输出低置信度。
- **流式背压**：固定容量包队列，队满阻塞生产者；下游解码/修复致命错误时取消队列并唤醒等待线程，内存随队列容量有界。
- **可证明对齐**：剪辑点与 seek 均取双向最近邻，报告目标与实际落点误差；索引经临时文件、rename、完成标记三步提交，截断输出不会误报成功。
- **可复现**：xorshift 确定性 RNG，同参数重复生成的侧车逐字节一致，索引可重复生成与验证。

## 输出字段

`packets_in`、`packets_repaired`、`wraps`、`drift_ppm`、`queue_peak`、
`target_bitrate_bps`、`actual_bitrate_bps`、`bitrate_error_pct`、`psnr_db`、
`manifest_alignment_ms`、`seek_error_ms`，精度以毫秒与码率整数计。
