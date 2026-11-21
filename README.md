# media_pipeline

区域广播多声道 PCM/WAV 素材的本地处理流水线：流式解析、静音检测、切分、
内容指纹判重、FFmpeg 再封装、检查点恢复与可追溯清单。纯终端、纯本地，
无外部服务依赖。

## 依赖（macOS，Homebrew 一条命令）

```bash
brew install cmake ffmpeg catch2
```

技术栈：C++20、CMake 3.28+、FFmpeg（libavformat/libavcodec/libswresample）、
Catch2 3.x。同时支持 Apple Silicon 与 Intel macOS。

## 构建

```bash
cmake -S . -B build -DCMAKE_PREFIX_PATH="$(brew --prefix)" -DCMAKE_BUILD_TYPE=Release
cmake --build build --parallel
```

## 自测与测试

```bash
# 生成夹具并跑通演示流水线
./build/media_pipeline selftest --generate-fixtures --work ./tmp

# Catch2 单元测试（恢复、损坏、判重、路径安全等）
ctest --test-dir build --output-on-failure
```

夹具由 `scripts/make_fixtures.sh` 与内置生成器自产，包含正常素材、尾部截断、
非整帧尾块与重复内容，不引用任何外部素材。

## 处理命令

```bash
./build/media_pipeline process \
  --input ./tmp/stereo.wav --output ./tmp/out.m4a \
  --manifest ./tmp/manifest.json --checkpoint ./tmp/state.json \
  --bitrate 192000 --resume
```

重复同一命令即幂等续跑：检查点核对输入摘要（整文件 FNV 摘要、大小、格式、
总帧数）与全部作业参数，不匹配则拒绝续跑，匹配则恢复。

### 退出码

| 码 | 含义 |
|----|------|
| 0  | 验证通过 |
| 2  | 素材损坏但按 Skip 策略完成恢复（清单标注缺口） |
| 其他非 0 | 参数、输入或验证失败 |

## 关键设计

- **统一时间基准**：全程整数采样刻度（`media/time.hpp`），半开区间
  `[start,end)`，边界样本只归属一个片段，禁止浮点累计漂移。
- **结构化错误**：携带源路径、字节偏移、样本序号与阶段（`media/error.hpp`）。
- **流式 WAV 解析**：校验 RIFF/fmt/data 块、采样格式与声道布局；非整帧尾块
  与帧中途截断报告原始字节偏移，后续可解析块继续处理。
- **有界内存**：解析、静音、指纹、转码均按 4096 帧块流式推进，指纹仅缓存
  一个窗口；2 小时 8 声道素材峰值内存远低于 256 MiB。
- **再封装**：经 libswresample 转换采样率/声道，AAC 优先 macOS `aac_at`
  以贴合目标码率，实测码率按输出文件字节与整数帧时长推导。
- **清单对齐**：输出时间线由非重复片段半开区间首尾相接拼成，
  `manifest_alignment_error_samples` 非 0 即验证失败。

## 模块

| 文件 | 职责 |
|------|------|
| `src/wav_reader.cpp` | 流式 WAV 块解析 |
| `src/silence.cpp` | 静音区间与峰值/RMS/削波统计 |
| `src/segmenter.cpp` | 时长/静音/最大样本数切分 |
| `src/fingerprint.cpp` | 窗口指纹与判重 |
| `src/transcoder.cpp` | libswresample + FFmpeg 再封装 |
| `src/checkpoint.cpp` | 原子检查点与续跑核对 |
| `src/manifest.cpp` | 可追溯清单与时间线验证 |
| `src/pipeline.cpp` | 各阶段编排 |
| `app/main.cpp` | 终端适配器 |
