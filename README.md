# Aircheck

电视台母版车间的多轨素材检查与无损再封装工具。纯本地运行，无网络依赖。

## 环境

```sh
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

## 常用命令

```sh
.venv/bin/python -m aircheck.cli demo \
  --formats mov,mp4,mkv --queue-size 4 --slow-consumer-ms 25 --output /tmp/aircheck-out

.venv/bin/python -m aircheck.cli verify --input /tmp/aircheck-out --json
```

退出码：`0` 通过，`1` 验收差异，`2` 参数或输入错误，`3` 不可恢复处理失败。
