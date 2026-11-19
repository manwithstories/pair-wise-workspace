# media_batch — 电视台母版批处理工具

纯本地、离屏的多轨时间线媒体批处理工具：解析 SRT/WebVTT 字幕与多制式时间码，
合成多轨时间线，经有界队列流式处理与分块检查点，调用 FFmpeg 解封装/解码/再封装，
离屏导出 PNG 复核帧序列，并汇总码率与质量指标为终端 JSON。无图形界面、无外部服务。

## 技术栈与依赖（macOS）

| 依赖 | 用途 | 安装 |
| --- | --- | --- |
| CMake ≥ 3.25 | 构建 | `brew install cmake` |
| FFmpeg 7.x/9.x（libavformat、libavcodec、libavutil、libswresample） | 媒体编解码与封装 | `brew install ffmpeg` |
| Catch2 3.x | 单元/集成测试 | `brew install catch2` |
| Apple Clang（支持 C++20） | 编译器 | 随 Xcode Command Line Tools 提供 |

一次性安装：

```sh
brew install cmake ffmpeg catch2
```

支持 Apple Silicon 与 Intel（仅用可移植 C++20 与 FFmpeg 稳定 API，无架构专属代码）。

## 构建与测试

```sh
cmake -S . -B build -DCMAKE_PREFIX_PATH="$(brew --prefix)"
cmake --build build
ctest --test-dir build --output-on-failure
```

## 演示（样例数据全部由程序生成，不引用外部文件）

合格批次（退出码 0，指标全部合格）：

```sh
./build/media_batch demo --work demo-run --target-bitrate 4000000 \
  --chunk-frames 120 --queue-capacity 8 --slow-consumer-ms 5 \
  --emit-frames 12 --json
```

损坏批次（截断字幕、错误时间码、字节损坏媒体；退出码 2，报告仍生成）：

```sh
./build/media_batch demo --work damaged-run --fixtures damaged --resume --json
```

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 处理完成且指标合格 |
| 2 | 输入含可恢复错误，但报告已生成 |
| 3 | 检查点或输入校验失败（索引损坏/截断/输入摘要变化） |
| 4 | 输出指标超限（码率偏差、PSNR、字幕对齐、队列容量） |
| 5 | 构建或参数错误 |

## 关键命令行参数

`--work DIR` 工作目录（所有产物被约束在其内）、`--fixtures good|damaged`、
`--resume` 续跑（无检查点时先建立再重跑以演示恢复）、`--json`、
`--target-bitrate N`、`--container matroska`、`--chunk-frames N`、
`--queue-capacity N`（硬性上限 256）、`--slow-consumer-ms N`、`--emit-frames N`。

## 架构与数据流

```
字幕/时间码解析 ──(统一 90kHz 整数刻度)──▶ 多轨时间线合成
   (subtitle/timebase)                     (timeline：优先级/边界裁定)
                                                  │
                                                  ▼
        分块检查点 ◀── 有界队列(背压) ◀── 解封装/解码/再封装 (media)
        (checkpoint)        (stream)                 │
                                                     ▼
                          离屏 PNG 帧序列 + 帧清单 (render)
                                                     │
                                                     ▼
                          报告聚合与退出码裁定 (report)
```

- 统一时间基准固定 90 kHz，可整除常见帧率，全程整数乘除，禁止浮点累积漂移。
- 有界队列固定容量，慢消费者下生产者阻塞而非膨胀，报告峰值长度/字节/内存。
- 检查点原子写块（tmp+rename），索引带整体 SHA-256，任何篡改/截断/输入摘要
  变化都拒绝静默复用并返回可定位错误。
- 帧清单记录统一刻度、源位置、帧号与每帧 SHA-256；缺帧、重复采样、末段不足
  整帧均明确统计。

## 模块（src/）

`util`（错误模型/SHA-256/JSON/路径约束）、`timebase`、`subtitle`、`timeline`、
`stream`、`checkpoint`、`media_make`/`media_proc`、`render`、`report`、
`fixtures`、`pipeline`、`main`。测试见 `tests/pipeline_tests.cpp`。

## 有界限制

最大轨道数 16、字幕条目 50000、单条字幕 2048 字节、队列容量 256；超限即有界失败。
