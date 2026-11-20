"""CLI 验收命令测试：退出码、可核对字段与重复运行一致性。"""

import json
import re
from pathlib import Path

from mediasync.cli import main


def _run(capsys, *argv):
    code = main(list(argv))
    out = capsys.readouterr().out
    return code, out


def test_demo_success_prints_all_verifiable_fields(tmp_path, capsys):
    work = str(tmp_path / "work")
    code, out = _run(
        capsys,
        "demo", "--work", work,
        "--corrupt-every", "7",
        "--interrupt-after", "3",
        "--resume",
        "--representations", "3",
        "--segment-ms", "500",
    )
    assert code == 0, out

    def field(name):
        match = re.search(rf"^{name}:\s*(.+)$", out, re.MULTILINE)
        assert match, f"缺少字段 {name}"
        return match.group(1).strip()

    corrupt = int(field("corrupt_offset_bytes"))
    assert corrupt >= 0
    error_bytes = int(field("corrupt_locate_error_bytes"))
    assert error_bytes <= 1  # 与生成损坏位置误差不超过 1 字节
    assert int(field("resume_ms")) < 500
    assert int(field("segments")) >= 2
    assert field("aligned_representations") == "3/3"
    assert re.fullmatch(r"[0-9a-f]{64}", field("manifest_sha256"))
    assert re.fullmatch(r"[0-9a-f]{64}", field("index_sha256"))


def test_demo_repeated_runs_have_identical_digests(tmp_path, capsys):
    work = str(tmp_path / "work")
    argv = ["demo", "--work", work, "--representations", "3", "--segment-ms", "500"]
    code1, out1 = _run(capsys, *argv)
    digest1 = re.findall(r"(?:manifest|index)_sha256:\s*([0-9a-f]{64})", out1)

    # 删除产物保留输入，重新生成并运行。
    for name in ("manifest.json", "index.json"):
        (Path(work) / name).unlink()
    code2, out2 = _run(capsys, *argv)
    digest2 = re.findall(r"(?:manifest|index)_sha256:\s*([0-9a-f]{64})", out2)

    assert code1 == code2 == 0
    assert digest1 == digest2 and len(digest1) == 2


def test_demo_bad_arguments_return_exit_code_two(tmp_path, capsys):
    code, out = _run(capsys, "demo", "--work", str(tmp_path / "w"),
                     "--representations", "9")
    assert code == 2 and "参数错误" in out

    code, _ = _run(capsys, "demo", "--work", str(tmp_path / "w"),
                   "--segment-ms", "0")
    assert code == 2


def test_demo_path_traversal_rejected(tmp_path, capsys):
    code, out = _run(capsys, "demo", "--work", "../../etc/evil")
    assert code == 2
    assert "越界" in out


def test_demo_outputs_manifest_and_index_files(tmp_path, capsys):
    work = Path(tmp_path) / "work"
    code, _ = _run(capsys, "demo", "--work", str(work), "--representations", "3")
    assert code == 0
    manifest = json.loads((work / "manifest.json").read_text())
    index = json.loads((work / "index.json").read_text())
    assert manifest["fully_aligned"] is True
    assert manifest["total_representations"] == 3
    assert index["records"] and "input_digest" in index
