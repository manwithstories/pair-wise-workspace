# subqc

`subqc` 是面向 SSA/ASS 剧集字幕的终端批量体检工具。它自行解析样式继承链、事件和覆盖标签，再把解析结果规范化为独立样式表交给 libass 离屏排版，输出几何摘要与位图 CRC32，不落盘图片。

## 构建

依赖：C++17 编译器、CMake 3.20+、pkg-config、FreeType、HarfBuzz、FriBidi。libass 0.17.1 以源码包固定在 `vendor/libass-0.17.1.tar.xz`，构建时静态编译到 `build/`，不依赖系统 libass。`vendor/nlohmann/json.hpp` 为 nlohmann/json 3.11.3。

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
```

## 使用

生成确定性夹具和内置字体：

```sh
./build/subqc gen-fixtures --out fixtures --events 240 --seed 20261114
```

批量扫描并输出文本摘要和 JSON 报告：

```sh
./build/subqc scan \
  --input fixtures --width 1920 --height 1080 \
  --font-dir fixtures/fonts --json out/report.json --text
```

`scan` 还支持 `--safe-x` 和 `--safe-y`，未指定时按画面宽高的 5% 计算安全边距。输出路径的父目录会自动创建。

## 解析与渲染规则

- 支持 UTF-8 BOM、CRLF、动态 `Format:` 列和重复样式；同名样式后者覆盖并列 warning。
- 样式按父链从根到叶合并，支持字号、颜色、边距、描边等字段；缺失父样式回落 `Default`。样式环使相关样式失效并计入 errors。
- PlayRes 缺失时使用 384x288。样式链同时输出原始单位 `font_size`/`outline` 和目标分辨率像素值 `font_size_px`/`outline_px`。
- 覆盖标签支持 `\r \b \fs \pos \an \fad \t` 及常见 ASS 标签计数；未知标签只计数。`\{` 作为普通文本，不进入标签计数。
- `\r` 切换命名样式，`\t(...)` 内部标签只计数、动画结束后不泄漏；每段文本输出生效样式。
- 每个事件在独立时间槽渲染一帧，按 alpha 阈值求并集包围盒；`crc32` 对完整 1080p alpha 画布计算，保证重跑一致。
- 字体只从 `--font-dir` 递归读取，通过 `ass_add_font` 注入内存字体，libass 使用 `ASS_FONTPROVIDER_NONE`；不查询系统字体或网络。

## 报告与退出码

JSON 顶层包含 `summary`、`files`、`style_chains`、`inline_tag_counts`、`unknown_tag_counts`、`render`、`margin_violations`、`warnings` 和 `errors`。`render` 每项包含 `event/style/x0/y0/x1/y1/w/h/glyphs/crc32` 及分段文本。

退出码：

- `0`：无告警、无越界、无失效样式。
- `1`：存在安全边距越界、warning 或非致命样式错误。
- `2`：命令行参数或输出路径错误。
- `3`：存在无法读取/分节的致命文件；批次其余文件仍会完成处理。

运行完整验收断言：

```sh
scripts/acceptance.sh
```
