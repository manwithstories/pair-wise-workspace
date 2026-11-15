# mp4qc

卫视/长视频母版入库前的 ISO BMFF（含 fragmented MP4）索引核对与重建
命令行工具。纯 Python 标准库实现（struct / mmap / json / argparse），
不依赖播放框架，不调用 ffmpeg，流式游标解析，mdat 不进内存。

## 子命令

```sh
python -m mp4qc fixtures --out fixtures --seed 7      # 生成六类样例媒体
python -m mp4qc sample-table FILE [--track N] [--json OUT]
python -m mp4qc verify FILE [--json OUT] [--strict]
python -m mp4qc rebuild FILE --out OUT [--json REPORT]
```

- `fixtures`：生成正常、索引漂移、截断、多轨、co64、stz2 六类夹具。
- `sample-table`：展开 stts/ctts/stsc/stsz/stz2/stco/co64/stss 为逐样本
  时长与字节区间；多轨文件未指定 `--track` 时逐轨并列统计。
- `verify`：将 sidx 引用区间与真实 moof 边界逐条比对，输出每条偏差
  字节、最大漂移与首条违规序号；`reference_type=1` 递归展开一层。
- `rebuild`：按真实边界重算子段并回填 sidx（32 位溢出转 version 1），
  写出后立即复检。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 全部通过 |
| 2  | 存在 error 级违规 |
| 3  | 文件截断或无法解析 |
| 4  | 重建后复检未通过 |

## 测试

```sh
python -m pytest
```

