# mediaprobe

纯命令行、离线的宣发素材入库校验工具。所有容器读取均由标准库 `struct/io`
实现，运行期不调用 ffmpeg、ffprobe、pymediainfo 或 mutagen。

## 安装与测试

```bash
python -m pip install -r requirements-dev.txt
python -m pytest
```

目标运行版本为 Python 3.11+；当前代码不依赖 3.12 专属语法。

## 快速验收

```bash
python -m mediaprobe gen --out fixtures --count 14 --seed 7
python -m mediaprobe probe fixtures/trailer_1080_av.mp4 --json out/probe.json
python -m mediaprobe check fixtures --profile profiles/delivery.json \
  --tolerance-ms 40 --json out/check.json
python -m mediaprobe pack fixtures --profile profiles/delivery.json \
  --bundle build/bundle_0001 --db build/index.sqlite3 --json out/pack.json
python -m mediaprobe index issues --db build/index.sqlite3 --severity blocking
```

`check` 和 `pack` 在存在阻断项时返回 1；这是验收样本的预期行为。

## 命令

- `probe <file> [--json path]`：输出容器、呈现时长、轨道参数和片段状态。
- `check <file|dir> [--profile path] [--tolerance-ms 40] [--json path]`：按投递档执行规则。
- `pack <file|dir> --bundle DIR --db FILE [--json path]`：按 SHA-256 幂等复制通过资产，
  按档位 `slice` 选择器写 `tracks/<sha>/<track_id>.es`，并生成确定性 manifest。
- `index issues --db FILE [--severity blocking|info|all]`：查询审计索引。
- `gen --out DIR --count 14 --seed 7`：生成确定性的 14 个验收夹具。

索引包含 `assets`、`tracks`、`issues`、`runs` 四张表。重复导入使用 upsert，
同一 bundle 重跑不会增加行数；写入在单事务内完成，失败会整批回滚。

## 退出码

| code | 含义 |
| --- | --- |
| 0 | 全部通过 |
| 1 | 存在阻断项 |
| 2 | 参数或 IO 错误 |
| 3 | 容器无法解析 |
| 4 | 索引写失败 |

独立错误码包括 `FILE_EMPTY`、`FILE_TRUNCATED`、`MAGIC_UNKNOWN`、
`PERMISSION_DENIED`、`BOX_MALFORMED`、`FRAG_NO_DEFAULT_DURATION` 等。

## 有界读取

盒扫描只读取头部并在父盒字节区间内前进，`mdat` 等未知大盒按长度跳过；
SHA-256、复制和切片均使用固定大小块。样本表读取设置了单盒上限，避免把媒体
负载整文件读入内存。

## 源码布局

```text
mediaprobe/
  boxes.py      ISO BMFF 盒读取与区间
  isobmff.py    轨道、时长、样本表、moof/traf/trun
  riff.py       WAV/RIFF、奇数填充、扩展子格式
  codecs.py     avcC、hvcC、esds/AAC 配置
  probe.py      魔数分派与统一 MediaAsset
  checks.py     交付规则引擎
  index.py      SQLite 四表事务
  report.py     终端表格与 JSON
  packager.py   幂等打包、manifest、切片
  cli.py        click 子命令
scripts/
  gen_samples.py
  boxwrite.py
tests/
profiles/delivery.json
```
