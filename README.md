# audiocut

方言语音长录音自动切分工具：WAV 经 `wavio → framing → noise → detector`
两遍按块扫描后写入 SQLite，再由 `report` / `export` 输出结果。
仅依赖 NumPy 与标准库，纯终端形态。

## 安装与演示

```sh
pip install -r requirements.txt
python scripts/gen_fixtures.py --out fixtures --seed 20261114
python -m audiocut calibrate --input fixtures/calib_a.wav --out profiles/default.json --frame-ms 20 --hop-ms 10
python -m audiocut scan --input fixtures --db build/index.sqlite --profile profiles/default.json
python -m audiocut report --db build/index.sqlite --json
python -m audiocut export --db build/index.sqlite --profile profiles/default.json --out build/labels.jsonl
```

## 子命令

- `calibrate`：由校准文件估计噪声底，产出带版本与校验和的档案。
- `scan`：按路径排序递归扫描目录，sha256 幂等重扫，单文件失败隔离。
- `report`：语段数、起止采样点、静音总量，并校验语段+静音=总样本。
- `export`：按档案导出标签清单（jsonl）。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 2 | 参数错误 / 输出路径越界 |
| 3 | 输入格式不支持（浮点/压缩/位深） |
| 4 | 档案无效或与采样率不匹配 |
| 5 | 批处理部分失败 |
| 130 | 手动中断 |

## 测试

```sh
python -m compileall audiocut scripts tests
python -m pytest tests/
```
