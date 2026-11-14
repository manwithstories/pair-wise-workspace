# frameaudit

离线逐帧审计、时间轴修复与 CFR 代理片生成工具。面向 H.264/H.265、VFR、B 塔重排、
重复/回退 PTS 与跨段断档素材；所有样片由代码合成，不依赖外部素材或服务。

## 安装

```bash
python3.11 -m venv .venv
. .venv/bin/activate
pip install -e .
pytest -q
```

`pyproject.toml` 锁定 `av==12.3.0`、`numpy==1.26.4`、`pytest==8.3.5`。

## 命令

```bash
python -m frameaudit fixtures --out fixtures --seed 7
python -m frameaudit scan fixtures/vfr_bpyramid.mp4 --json
python -m frameaudit repair fixtures/vfr_bpyramid.mp4 \
  --emit-index out/index.jsonl --emit-proxy out/proxy.mp4 --json
python -m frameaudit verify out/index.jsonl out/proxy.mp4 --json
```

`scan` 解码整片一次，按真实呈现序 PTS 统计 I/P/B、关键帧与帧间隔。它不会用标称帧率推算
VFR 间隔。

`repair` 先以可配置深度（默认 16）的小窗口按 PTS 重排。同 PTS 保持解码序，缺失 PTS 在
前后已知时间戳之间插值并单独计数。随后以探测得到的真实中位帧间隔重建严格单调时间轴，
区分 `duplicate_pts`、`pts_rollback`、`gap`、`segment_jump` 与 `missing_pts`。

索引逐行包含：

```text
decode_idx, present_idx, pts_raw_us, pts_fixed_us,
pict_type, keyframe, plane_hash
```

代理片与索引使用同一份修复帧流，输出为恒定帧率的无损 MP4。无损编码保证回读时
亮度平面哈希序列可与索引逐帧核对。

`verify` 流式对齐索引行与代理帧，核对数量、首末 PTS、严格单调性、CFR 间隔和哈希序列。
坏包、截断包或无法转换的像素格式会记录在 `bad_regions`；已解码帧仍会继续处理，坏包边界
处会先刷新重排窗口。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 2 | 参数或输入错误 |
| 3 | 解码或写盘失败 |
| 4 | 索引与代理校验不一致 |

JSONL 使用固定字段顺序和紧凑 JSON，同输入同参数重复运行的索引文件逐字节一致。

