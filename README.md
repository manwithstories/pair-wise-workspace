# h264probe

离线 H.264 Annex B 裸流结构探针：把码流结构（参数集、切片、帧、GOP）
转成可入库、可比对(JSON / diff)的基线报告。仅依赖 C++17 标准库，
不链接 FFmpeg / libavcodec / OpenH264 等现成解析库；单线程、流式扫描，
200MB 输入峰值内存远低于 64MB、耗时远低于 4s（实测约 0.05s / 3MB）。

## 构建

```sh
cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
ctest --test-dir build --output-on-failure
```

## 生成夹具

样例码流由 `tools/gen_fixtures.py`（Python 3 标准库）现场生成，不引用外部文件：

```sh
python3 tools/gen_fixtures.py --out fixtures --seed 7 --gops 24 --slices 1,3,5 --interlace paff
python3 tools/gen_fixtures.py --out fixtures --seed 7 --large-mb 200
```

`--interlace` 支持 `progressive` / `paff` / `mbaff`；`--recovery-every K`
让每第 K 个 GOP 以恢复点 SEI（替代 IDR）开组；`--with-b` 混入 B 帧。

## 使用

```sh
./build/h264probe analyze --input fixtures/paff_3slice.h264 --format json --out out/baseline.json
./build/h264probe analyze --input fixtures/large.h264 --format text --stats
./build/h264probe diff --left out/baseline.json --right out/other.json
```

- `analyze`：`--format json|text`（默认 json），`--out` 缺省写 stdout，
  `--stats` 附加总耗时与峰值内存（文本模式每个 GOP 一行）。
- `diff`：字段级比对两份报告（忽略 `file` 与 `stats` 段），
  列出漂移字段 `path: left -> right`。

## 报告字段

- `sps`：profile_idc、level_idc、width、height、chroma_format_idc、
  frame_mbs_only_flag、mbaff、fps（VUI 时序推导）
- `counts`：I / P / B / other / total（slice_type 0..9 归一化：
  {2,5,7}->I，{0}->P，{1,6}->B，SP/SI->other；互补场对计一帧）
- `gops[]`：index、start_byte_offset（绝对偏移）、au_index、frame_count、
  type_mix、poc_start；IDR 或恢复点 SEI 开新组
- `slices`：per_picture_histogram（每图切片数直方图）、ue_avg_bits
  （切片头 ue 平均码长）、header_bits_total（切片头总比特数）
- `warnings`：dup_first_mb、cross_pps、slice_count_surge、
  missing_field_pair、duplicate_field、zero_length_nals 等

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功（diff：两份报告一致） |
| 2 | 参数错误 |
| 3 | 码流解析失败（截断、非法 ue、起始码异常、引用缺失） |
| 4 | 不支持的档次或特性（SVC/MVC、separate colour plane、FMO 等） |
| 5 | 基线漂移（diff 发现字段差异） |

## 模块

`src/bitreader`（位流/指数哥伦布）→ `src/annexb`（流式起始码扫描）→
`src/nal` → `src/sps` / `src/pps` / `src/slice` / `src/sei` →
`src/picture`（归并、场对、GOP）→ `src/report`（JSON/文本）；
`src/json` + `src/diff` 只读报告不回读码流；`src/analyzer` 为管线粘合，
`src/main.cpp` 提供子命令与退出码。
