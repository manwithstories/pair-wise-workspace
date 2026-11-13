# mediaidx

本地、只读扫描的广告素材感知去重工具。判定不依赖文件名、扩展名、大小或整文件哈希；这些信息只用于增量扫描和身份记录。

## 安装与生成样例

```bash
python3.11 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt

python fixtures/generate_corpus.py --force
```

夹具脚本使用 NumPy、Pillow、soundfile 和 imageio-ffmpeg 现场合成图像、音频、视频、损坏件、隐藏目录、零字节文件与符号链接环，不引用外部媒体。

## 索引与报告

```bash
python -m mediaidx.cli index \
  --root fixtures/corpus \
  --db out/index.db \
  --report out/report.json \
  --jobs 4
```

固定终端汇总：

```text
素材总数=30 解码成功=27 解码失败=3 重复簇数=7 去重后独立素材=10 候选对=77 精验通过=37 否决=40 冲突率=0.00%
```

`out/report.json` 包含顶层 `summary`、按 `image`/`audio`/`video` 分组的 `groups`、完整 `clusters`、扫描跳过计数、解码调用数、缓存命中和精验统计。

其他子命令：

```bash
python -m mediaidx.cli inspect fixtures/corpus/delivery/audio/a_master.wav
python -m mediaidx.cli stats --db out/index.db
```

## 契约与算法

- `canonical.py` 只按魔数识别 PNG/JPEG/WebP、WAV/FLAC/OGG、MP4/MOV/MKV；路径校验拒绝语料根之外的路径与符号链接，扫描跳过隐藏目录。
- `loaders/` 提供显式解码注册表。解码失败被分类并写入 `quarantine`，不会中断批次。
- 图像：EXIF 方向校正、透明区域压平到白色、DCT 64 位 pHash + dHash；精验同时看 DCT 细节相关性与缩略图结构。
- 音频：声卡格式无关地下混为单声道、NumPy 频域重采样到 16 kHz、去除首尾静音。指纹由频带能量谱和短时频带序列组成；精验允许 250 ms 首裁，通过互相关求偏移后逐帧比较，并检查时长比与偏移边界。
- 视频：等距抽 16 帧供对齐使用，其中 8 帧组成帧序列签名；允许首尾各 1 秒裁切，按时间偏移逐帧对齐后取中位相似度。每帧复用图像规范化和指纹路径。
- LSH：所有 64 位候选哈希切成 4 个 16 位段建桶，只对共享桶的素材做精验。候选对是所有桶内组合的去重集合。
- 聚类：只有精验通过的对才会进入并查集；簇号是簇内最小内容 SHA-256。每轮从当前文件和当前精验图重建簇，因此删除桥接文件会自然拆簇并回收孤立成员。
- 增量：扫描始终按 1 MiB 分块重算内容 SHA-256，可发现大小与 mtime 未变的内容替换；内容未变时直接复用持久化指纹，`decode_calls=0`。候选桶、精验结果和最终簇均在事务中替换，不会留下半写簇。

## 真值与冲突率

冲突率定义为：精验判同但真值标签不同的对数 / 精验判同对数。没有真值清单时报告为 `0.00%`，但不会借此放宽任何精验；夹具默认自动读取 `fixtures/corpus.labels.json`，也可用 `--labels` 指定。当前合成夹具中所有正样本同簇、困难负样本不合并，冲突率为 `0.00%`。

## 测试

```bash
python -m unittest discover -s tests -v
```

测试覆盖：

- 魔数与扩展名误导、符号链接拒绝、分块哈希替换检测；
- 图像跨格式、半尺寸、EXIF 旋转、透明压平与负样本拒绝；
- 音频重采样、下混、增益、OGG、250 ms 首裁和时长越界；
- 64 位哈希四段候选桶、事务回滚、WAL、链式簇删除桥接成员后的拆分；
- 端到端 1/4 并发结果一致、重跑零解码、同大小同 mtime 替换与删除回收。

仓库不包含运行时数据库或缓存；`out/`、生成语料与虚拟环境均被忽略。
