# Pairwise Offline Bake

纯终端、完全离屏的 P6 PPM 序列烘焙与逐帧核验工具。项目只使用 C++17 标准库和
Python 3.10 标准库，不依赖图形窗口、浏览器、摄像头、网络或第三方包。

## Build

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j4
./build/kernel_tests
```

两个目标均固定使用 `-O2 -Wall -Wextra -Werror -fno-fast-math
-ffp-contract=off`。`transition_bake` 提供烘焙、核验、报告和自测入口，
`kernel_tests` 是无框架的独立断言程序。

## End-to-end demo

```sh
python3 tools/gen_fixture.py --out f --width 64 --height 48 \
    --clips 4 --frames 36 --seed 7
./build/transition_bake render --timeline f/tl.txt --src f/frames \
    --out out --seed 7 --report r.json
./build/transition_bake verify --dir out --expect f/expect.json \
    --report v.json --strict
python3 tools/check_report.py --report v.json --strict
./build/transition_bake selftest --report s.json
```

所有子命令只在 stdout 打印一行 JSON，并把同一结构写入 `--report`。未提供
`--report` 时写入当前目录的 `report.json`。`render` 和 `verify` 都会输出五类
内核的逐类证据；`verify` 还会逐帧回读 PPM、重算期望帧并比较 FNV-1a 64 哈希。

## Commands

- `render --timeline TL --src ROOT --out DIR [--seed N] [--jobs N] [--report R]`
  逐帧烘焙 `DIR/000000.ppm`，文件名固定为六位零填充。`--jobs` 分片并行，
  输出与串行路径逐字节一致。
- `verify --dir DIR --expect EXPECT [--strict] [--continue-on-error]`
  严格检查帧数、文件名、尺寸和每帧哈希；`--strict` 还拒绝多余 PPM。
  `--continue-on-error` 会继续读取并收集所有坏帧。
- `report --dir DIR --timeline TL --src ROOT [--strict]`
  不依赖 expectation，直接对已有输出生成核验报告。
- `selftest [--report R]`
  检查五类内核端点、Q16.16 进度、时间映射、P6 往返和帧缓冲不变量。

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | 全部通过 |
| 1 | 帧数、文件名或哈希不符 |
| 2 | 时间线、变速参数、素材或 PPM 非法 |
| 3 | 帧或报告落盘失败 |
| 4 | 参数解析失败 |
| 5 | 内核端点恒等或帧缓冲不变量被破坏 |

## Timeline format

`tools/gen_fixture.py` 生成 JSON 时间线。顶层包含 `width`、`height`、`seed`、
`speed`、`clips` 和 `transitions`；片段目录必须是相对路径且不能包含 `..`。

```json
{
  "speed": [
    {"frames": 6, "source": 0, "rate": [1, 2]},
    {"frames": 6, "source": 3, "rate": [2, 1]}
  ],
  "clips": [
    {"id": "c0", "dir": "c0", "frames": 36, "start": 0},
    {"id": "c1", "dir": "c1", "frames": 36, "start": 32}
  ],
  "transitions": [
    {"from": 0, "to": 1, "class": "linear_blend",
     "start": 32, "frames": 4, "priority": 1}
  ]
}
```

轨道总帧数为各片段长度之和减去转场重叠帧数。相邻片段的重叠窗口必须与一条
转场记录完全一致；转场窗口重叠会在报告错误中给出段号。源帧解析全部走
int64 有理积分，`TimePoint` 同时给出整数源帧与 Q16.16 小数进度。分母为零、
倒放越过源起点、源帧越界都带帧号失败。

变速调度包含 `1/2`、`2/1`、`3/1`、`0` 停顿与 `-1` 倒放。时间映射在区间
边界按精确有理位置校验，避免跳帧或重复积分。渲染时停顿帧使用同一源帧与
零进度，因此哈希相同；报告把这类确定性重复计为 `intentional_holds`，
`duplicate_frames` 只统计非预期重复。

## Kernel and report model

五类内核都通过同一注册表和 `applyKernel` 调度：

- `linear_blend`：Q16.16 逐通道定点混合。
- `threshold_mask`：均匀像素阈值掩膜。
- `channel_key`：绿色通道键控阈值。
- `geometry_resample`：双线性几何重采样与硬边选择。
- `block_permute`：按块哈希阈值的块置换。

`applyKernel` 在进度 `0` 和 `65536` 直接复制前后帧，并检查目标帧所有像素都已
初始化。时间线渲染器和 `selftest` 使用这同一个入口，不复制混合实现。

报告中的逐类字段为：`frame_count`、`first_frame_hash`、`last_frame_hash`、
`mean_abs_diff`、`max_abs_diff`、`changed_pixels`、`diff_histogram`、
`diff_peak_frame`。附加的 `diff_curve` 记录相邻探针帧的平均绝对差：
线性混合呈单峰且端点归零，硬边类在中段近似平台。`check_report.py` 会复核
这些范围与形态，而不只看退出码。

## Determinism and limits

- 哈希使用自实现 FNV-1a 64，序列化只包含尺寸与 RGB 字节，不包含行跨距填充。
- 所有时间算术为 int64 有理数，无浮点累积；编译关闭快速数学和浮点收缩。
- 相同 seed、串行/`--jobs`、重复运行以及 `-O0`/`-O2` 构建产生相同帧哈希。
- 64×48、132 帧的完整烘焙实测约 0.03 秒，峰值内存约 4 MB。
- 帧路径固定为 `out/000000.ppm` 形式，不接受用户提供的路径分量越界。
- 生成目录 `build/`、`out/`、`f/`、PPM 与报告均被 `.gitignore` 排除。
