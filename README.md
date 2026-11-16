# lutgrade

CG 过场动画调色一致性核验工具：解析 `.cube` / `.3dl` / `.spi3d` 三种 3D
LUT，用同一插值内核跑全浮点离线路径与 12 位定点引擎快速路径，核验两条
路径落在同一误差带内，并输出直方图与 JSON 报告。纯 C++17 自研，无第三方
依赖。

## 构建

```sh
cmake -S . -B build
cmake --build build -j
ctest --test-dir build
```

## 子命令

- `demo --out-dir out` 生成恒等/风格化 LUT、渐变图与 48 帧序列，并用恒等
  LUT 自检（偏差为零），打印 256 桶直方图。
- `probe --lut f.cube` 打印 `grid`、`domain_min/max`、`format`、`order`。
- `apply --lut f.cube --input in.ppm --output out.ppm --report r.json`
  浮点路径出图，与定点路径对比，输出 max_dev/rmse/psnr 与直方图报告。
- `verify --lut f.cube --frames dir --tolerance 0.002` 逐帧比较两条路径。
- `batch --lut f.cube --input-dir d --output-dir o [--jobs N]` 线程池并行，
  输出顺序固定，失败帧记名后继续。

通用选项：`--interp trilinear|tetrahedral`、`--edge clamp|extend`、
`--in-cs/--lut-cs/--out-cs rec709|rec2020|srgb|acescg`、
`--no-input-transform`、`--no-output-transform`。

## 帧格式

`.ppm`（P6/P3，8/16 位）、`.pam`（P7 RGB）、`.ff`（自定义 float32：
`FF\n<w> <h>\n` + 原始数据）。浮点帧含 NaN 时报内部一致性错误。

## 退出码

0 成功；2 参数错误；3 LUT 解析失败；4 读写失败；5 容差超限；6 内部一致
性失败（NaN 像素、奇异矩阵等）。
