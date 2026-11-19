# region_media

跨节点 HLS/DASH 传输媒体质量核验工具（纯本地、终端形态）。

## 安装与验收

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install 'av==12.3.0' 'numpy==2.1.3' 'soundfile==0.12.1'
.venv/bin/python -m region_media.cli generate --out .tmp-fixtures
.venv/bin/python -m region_media.cli process --manifest .tmp-fixtures/master.m3u8 \
    --index .tmp-index.sqlite --chunk-size 16 --resume
.venv/bin/python -m region_media.cli verify --suite generated --json
```

退出码：`0` 验收通过，`1` 处理或指标失败，`2` 参数或输入无效。

## 模块

`manifest` 清单解析与引用校验 · `ingest` 解复用与回绕/断流识别 ·
`clock` 漂移估计与时基映射 · `buffer` 自适应抖动缓冲 ·
`recovery` 丢包/重复/缺口恢复 · `sync` 音画对齐 ·
`checkpoint` SQLite 事务检查点 · `metrics` 指标汇总。
