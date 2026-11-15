# smx

`smx` 是面向叙事游戏字幕的纯终端度量与预检工具。它用同一套 FreeType 26.6 定点字形度量完成单条测量和批处理，支持中日韩与拉丁混排、避头尾断行、贪心/DP 换行、行数截断以及稳定报告。

## 构建

依赖已随仓库提供：`vendor/freetype` 为 FreeType 2.13.2 源码，构建时只编译一次。运行时无外部服务依赖。

```sh
cmake -S . -B build && cmake --build build -j
```

产物：

- `libsmx.a`：核心库
- `smx`：命令行
- `smx_selftest`：C++ 自检程序（也可运行 `smx selftest`）

## 生成样例

```sh
python3 scripts/gen_fixtures.py
./build/smx genfont --out fixtures/synth.ttf --seed 7
```

`ttf_synth` 生成确定性的 TrueType 字体，包含 ASCII、全角标点、零宽/组合字符以及 200 个 CJK 字形。相同 `seed` 输出字节一致。`gen_fixtures.py` 只使用该字体覆盖的字形生成 120 条样例字幕。

## 命令

```sh
./build/smx measure \
  --font fixtures/synth.ttf --size 32 --max-width 640 --max-lines 2 \
  --input fixtures/cues.txt --out out/report.json

./build/smx batch \
  --font fixtures/synth.ttf --size 32 --max-width 640 \
  --dir fixtures/cues --out-dir out --jobs 4

./build/smx selftest
```

`measure` 可选参数：

- `--layout dp|greedy`：默认 `dp`
- `--truncate keep|ellipsis`：默认 `keep`
- `--fallback FILE[,FILE...]`：缺失字形回退链
- `--tsv FILE`：同时导出逐行 TSV

`keep` 保留超过 `--max-lines` 的自然行并报告 `LINES`；`ellipsis` 将内容压到上限并补 U+2026，省略号宽度参与判定。这个默认值与样例“120 条、约 340 行”的预检场景一致。

`batch` 按文件名排序读取目录内普通文件，每个文件独立打开字体并写出 `<文件名>.json`、`<文件名>.tsv`，最后写 `batch.json`。单文件失败不会中断其他文件，`files_failed` 汇总失败数。

## 字幕稿格式

解析器接受 UTF-8 BOM、CRLF 与注释，并保留输入行号用于报错。

```text
# 注释
STYLE default family=SMXSynth size=32
[style emphasis weight=bold]
[segment opening]

[cue c0001 style=emphasis segment=opening]
中文「字幕」，运行时 mixed text。
[cue c0002]
第一段硬换行\n第二段保持优先。
```

- `STYLE`、`[style ...]`：样式头；`STYLE` 同时设置后续 cue 的默认样式。
- `[segment ...]`：分段头。
- `[cue id ...]` 或 `## id`：开始一条 cue；空行结束当前 cue。
- 没有显式 cue 头时，每个非空文本行作为一条隐式 cue，也会识别常见 SRT 时间轴行并跳过。
- 文本支持 `\n`、`\t`、`\\`、`\uXXXX`、`\xHH` 转义。
- 非法 UTF-8 在解析时替换为 U+FFFD，并在报告中计数。
- 重复 ID、空 cue、未知指令等错误会给出原始行号。

## 排版与度量约定

- 字形加载固定使用 `FT_LOAD_NO_HINTING | FT_LOAD_NO_BITMAP`，advance 和包围盒均按 26.6 定点累计。
- kerning 只在相邻簇之间应用，行末最后一簇之后不计 kerning。
- 行高取 FreeType 像素尺寸的 `height`（ascender - descender + leading）。
- 组合附加符并入前簇；ZWSP/ZWNJ/ZWJ/变体选择符不占宽度，其中 ZWSP 产生断行机会。
- 空格、U+002D/U+2010 连字符、ZWSP、CJK 字间可断；NBSP 和 U+2011 不可断。
- 行首禁则：`、。，」』？！` 等关闭标点；行尾禁则：`（「『` 等开启标点。禁则位置不生成断点，排版自然回退到前一个合法机会。
- 硬换行始终优先，不被宽度合并。
- 不可断长串会强制切分，同时保留超出限制的字形包围盒并标记 `UNBREAKABLE`。
- 溢出类别：`OK`、`WIDTH`、`LINES`、`UNBREAKABLE`、`MISSING_GLYPH`；一条 cue 可同时有多个类别。

`cues[].lines[].start/end` 是簇索引，`end` 为开区间；`bbox` 为像素坐标，y 轴向下。`width_px` 与包围盒数值固定输出三位小数。

## 报告与摘要

`report.json` 使用固定键序，顶层包含：

```text
cues_total, cues_ok, overflow_total, overflow{width,lines,unbreakable,missing_glyph},
lines_total, max_line_width_px, invalid_utf8_total, report_hash, cues[]
```

每条 cue 包含文本、并集包围盒、行数、非法字节数、溢出类别、缺失字形次数/码点，以及逐行的 `start/end/width_px/bbox/break_reason`。

`report_hash` 为 64 位 FNV-1a 的 16 位小写十六进制。计算对象是移除了 `report_hash` 字段后的规范 JSON，因此同输入重复运行字节一致，不受线程数或时区影响。`batch.json` 使用同样的摘要规则。

当前样例结果为 120 条、341 行、`max_line_width_px` 640.000；样例刻意包含行数溢出、不可断长串、缺失字形和非法字节，因此 `measure`/`batch` 返回 2。

## 退出码

- `0`：成功且无溢出
- `2`：存在溢出条目
- `3`：字体或输入不可解析、读写失败
- `4`：自检断言失败
- `64`：参数错误

## 验证

```sh
ctest --test-dir build --output-on-failure
sh scripts/acceptance.sh
```

`scripts/acceptance.sh` 会重新生成字体与字幕夹具，执行 measure/batch/selftest，并检查样例汇总字段、行宽区间和摘要格式。性能路径不渲染位图、不保留 FreeType 位图槽；10,000 条样例在当前环境约 0.4 秒完成。
