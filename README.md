# 影视套底（Conform）命令行工具

把剪辑师导出的**决策表**与素材管理员维护的**素材清单**（同一自研交换格式）
转换为可执行的套底清单，并在素材改名、路径变化、时间基不同、变速与嵌套序列
场景下尽量恢复定位、显式报告偏差与冲突。纯 Python 3.11 标准库实现，无第三方
剪辑工程 SDK、无网络、无外部服务。

## 快速开始

```bash
python -m src.cli generate-fixture --dir .tmp_fixture
python -m src.cli reconcile \
  --decision .tmp_fixture/decision.txt \
  --manifest .tmp_fixture/manifest.txt \
  --root .tmp_fixture/media \
  --json-out .tmp_fixture/report.json
python -m src.cli verify --report .tmp_fixture/report.json
```

验收 fixture 的期望摘要：

```
回批条目数=12
已定位素材数=9
无法定位素材项=2
歧义项=1
时长偏差总帧数=3
最大偏差帧数=2
嵌套展开层数=3
```

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功且无业务错误 |
| 2 | 存在业务校验错误（素材缺失/歧义、源时长不足、零速、循环引用等） |
| 3 | 输入格式或参数错误（结构错误带行号） |
| 4 | 输出写入失败 |

`reconcile --strict`：任一业务错误即阻止写出结果（仍返回 2）。

## 模块结构

| 模块 | 职责 |
|------|------|
| `src/format_parser.py` | 自研块语法的**唯一**解析器；未知字段原样保留，结构错误带行号；流式生成器 |
| `src/model.py` | 不可变领域模型（frozen dataclass），时间基/速率统一用 `Fraction` |
| `src/import_decision.py` | 决策表导入，复用统一解析器 |
| `src/import_manifest.py` | 素材清单导入，复用同一解析器（禁止两套语法） |
| `src/resolver.py` | ID/别名链 → 规范化路径 → 大小+SHA-256 片段指纹多级定位；歧义不静默选择；沙箱与符号链接越界拒绝 |
| `src/timeline.py` | 嵌套序列递归展开、循环检测、父子来源链、变速源区间与取整漂移 |
| `src/reconcile.py` | 生成套底条目，检测缺失/时长不足/时间基/漂移/重叠 |
| `src/report.py` | 终端摘要、确定性 JSON、verify 独立复核 |
| `src/cli.py` | `generate-fixture` / `reconcile` / `verify` 子命令 |
| `tests/test_pipeline.py` | 夹具生成与全链路测试（pytest） |

## 交换格式（节选）

```
EXCHANGE version=1 timebase=24/1
  ASSET A01 path=clips/a01.mov size=2185 fingerprint=<sha256片段> duration=500
    ALIAS name=cam_alias target=A01
  END
  SEQUENCE S1 timebase=24/1
    TRACK V1
      CLIP c1 asset=A01 dst_in=0 dst_out=40 src_in=0 src_out=40
        SPEED dst_in=0 dst_out=15 rate=3/2
      END
    END
  END
END
```

- 时间码：`HH:MM:SS:FF` 或裸帧整数；时间基支持分数（如 `30000/1001`）。
- 速率：整数 / 小数 / 分数；负值表示反向；零速为业务错误。
- 块的 `END` 可省略（叶式书写），未知 `KEY=VALUE` 字段保留在 `extra` 中。

## 设计要点

- **确定性**：条目按稳定键排序并连续编号，JSON `sort_keys` 固定缩进，重复运行逐字节一致。
- **取整口径**：变速边界采用半向上取整（负数沿绝对值对称）；多段变速累计误差
  = 各段分别取整之和 − 精确总长整体取整，恒为整数帧。
- **安全沙箱**：仅访问 `--root` 内常规文件，拒绝符号链接目录/文件逃逸；不做真实素材扫描外的任何路径访问。
- **大清单**：解析为逐行生成器，文件内容不一次性载入；每个文件指纹仅计算一次并缓存。

## 测试

```bash
python -m pytest tests/ -q
```
