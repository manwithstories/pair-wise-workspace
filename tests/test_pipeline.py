"""端到端与单元测试：夹具生成 -> 两条导入 -> 展开 -> 定位 -> 回批 -> 报告 -> verify。"""

from __future__ import annotations

import json
from fractions import Fraction

import pytest

from src.cli import main as cli_main
from src.errors import ExitCode, FormatError
from src.fixture import generate_fixture
from src.format_parser import parse_fraction, parse_text, parse_timecode
from src.import_decision import import_decision_text
from src.import_manifest import import_manifest_text
from src.model import TimeBase
from src.reconcile import reconcile
from src.report import load_report_dict, verify_report
from src.resolver import AssetResolver, fingerprint_file, is_within, normalize_rel
from src.timeline import expand, half_up


# ---------- 纯函数：取整与时间码 ----------

def test_half_up_negative_and_positive():
    assert half_up(Fraction(1, 2)) == 1
    assert half_up(Fraction(-1, 2)) == -1
    assert half_up(Fraction(2, 3)) == 1
    assert half_up(Fraction(40, 3)) == 13
    assert half_up(Fraction(0)) == 0


def test_parse_fraction_forms():
    assert parse_fraction("3/2") == Fraction(3, 2)
    assert parse_fraction("-1") == Fraction(-1)
    assert parse_fraction("1.5") == Fraction(3, 2)
    assert parse_fraction("0") == 0


def test_timecode_and_bare_frames():
    tb = TimeBase.parse("24/1")
    assert parse_timecode("00:00:01:00", tb) == 24
    assert parse_timecode("120", tb) == 120
    with pytest.raises(FormatError):
        parse_timecode("00:00:01:24", tb)  # 帧分量越界
    with pytest.raises(FormatError):
        parse_timecode("00:00:01:00", TimeBase.parse("30000/1001"))


# ---------- 解析器：结构错误带行号、未知字段保留 ----------

def test_unknown_field_preserved():
    text = ("EXCHANGE version=1 timebase=24/1 custom=x\n"
            "  ASSET A1 path=a size=1 fingerprint=z owner=alice\n  END\nEND\n")
    doc = import_manifest_text(text)
    assert doc.extra["custom"] == "x"
    assert doc.assets[0].extra["owner"] == "alice"


def test_structure_errors_report_line():
    cases = [
        "EXCHANGE version=1 timebase=24/1\n  FOO X\n  END\nEND\n",      # 未知块
        "EXCHANGE version=1 timebase=24/1\nEND\nEXCHANGE version=1 timebase=24/1\nEND\n",
        "EXCHANGE version=1 timebase=24/1\n  ASSET A1 path=a size=1 fingerprint=z\n    TRACK T1\n    END\n  END\nEND\n",
    ]
    for bad in cases:
        with pytest.raises(FormatError) as ei:
            parse_text(bad)
        assert ei.value.line >= 1 or "根块" in str(ei.value)


def test_implicit_end_leaf_syntax():
    # CLIP/TRACK/SEQUENCE 省略 END 也应能正确成块
    text = ("EXCHANGE version=1 timebase=24/1\n"
            " SEQUENCE S timebase=24/1\n TRACK T\n"
            "  CLIP c asset=A1 dst_in=0 dst_out=10 src_in=0 src_out=10\n"
            "END\n")
    doc = import_decision_text(text)
    assert doc.sequences[0].tracks[0].clips[0].asset_id == "A1"


def test_clip_requires_asset_or_seq_exactly_one():
    good = ("EXCHANGE version=1 timebase=24/1\n SEQUENCE S timebase=24/1\n TRACK T\n"
            "  CLIP c dst_in=0 dst_out=10 src_in=0 src_out=10 asset=A1\n  END\n END\nEND\n")
    assert import_decision_text(good).sequences[0].tracks[0].clips[0].asset_id == "A1"
    bad = good.replace("asset=A1", "asset=A1 seq=S2")
    with pytest.raises(FormatError):
        import_decision_text(bad)


# ---------- 时间线：反向、零速、循环、多段漂移 ----------

def _decision(body: str) -> str:
    return ("EXCHANGE version=1 timebase=24/1\n" + body + "END\n")


def test_reverse_clip_source_interval():
    text = _decision(
        " SEQUENCE S timebase=24/1\n TRACK T\n"
        "  CLIP c asset=A1 speed=-1 dst_in=0 dst_out=10 src_in=50 src_out=40\n  END\n END\n")
    res = expand(import_decision_text(text))
    leaf = res.leaves[0]
    assert leaf.rate == -1 and leaf.src_in == 40 and leaf.src_out == 50


def test_zero_speed_is_business_error():
    text = _decision(
        " SEQUENCE S timebase=24/1\n TRACK T\n"
        "  CLIP c asset=A1 speed=0 dst_in=0 dst_out=10 src_in=0 src_out=10\n  END\n END\n")
    res = expand(import_decision_text(text))
    assert any(i.code == "zero_speed" for i in res.issues)


def test_cyclic_nested_sequence_detected():
    text = _decision(
        " SEQUENCE A timebase=24/1\n TRACK T\n  CLIP c1 seq=B dst_in=0 dst_out=5 src_in=0 src_out=5\n  END\n END\n END\n"
        " SEQUENCE B timebase=24/1\n TRACK T\n  CLIP c2 seq=A dst_in=0 dst_out=5 src_in=0 src_out=5\n  END\n END\n END\n")
    res = expand(import_decision_text(text))
    assert any(i.code == "cyclic_sequence" for i in res.issues)


def test_nested_chain_and_depth():
    text = _decision(
        " SEQUENCE A timebase=24/1\n TRACK T\n  CLIP c1 seq=B dst_in=0 dst_out=10 src_in=0 src_out=10\n  END\n END\n END\n"
        " SEQUENCE B timebase=24/1\n TRACK T\n  CLIP c2 asset=A1 dst_in=0 dst_out=10 src_in=0 src_out=10\n  END\n END\n END\n")
    res = expand(import_decision_text(text))
    leaf = res.leaves[0]
    assert leaf.sequence_chain == ("A", "B") and leaf.depth == 2


def test_multisegment_drift_accumulation():
    text = _decision(
        " SEQUENCE S timebase=24/1\n TRACK T\n"
        "  CLIP c asset=A1 dst_in=0 dst_out=50 src_in=0 src_out=50\n"
        "   SPEED dst_in=0 dst_out=15 rate=3/2\n"
        "   SPEED dst_in=15 dst_out=31 rate=3/2\n"
        "   SPEED dst_in=31 dst_out=50 rate=3/2\n  END\n END\n END\n")
    res = expand(import_decision_text(text))
    assert res.leaves[0].drift_frames == 1


# ---------- 定位器：沙箱、指纹、别名、歧义 ----------

def test_normalize_rel():
    assert normalize_rel("a\\b/./c/../d") == "a/b/d"
    assert normalize_rel("/x/y") == "x/y"


def test_is_within_blocks_symlink_escape(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    (root / "real.mov").write_bytes(b"data")
    outside = tmp_path / "outside.mov"
    outside.write_bytes(b"x")
    link = root / "link.mov"
    try:
        link.symlink_to(outside)
    except (OSError, NotImplementedError):
        pytest.skip("当前平台不支持符号链接")
    assert is_within(root, root / "real.mov")
    assert not is_within(root, link)


def test_fingerprint_single_read_determinism(tmp_path):
    f = tmp_path / "a.bin"
    f.write_bytes(b"abc123" * 100)
    h1 = fingerprint_file(f)
    h2 = fingerprint_file(f)
    assert h1 == h2 and len(h1) == 64


def test_alias_chain_and_ambiguity(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    (root / "clips").mkdir()
    (root / "dupes").mkdir()
    (root / "clips/a1.mov").write_bytes(b"A1-content")
    (root / "dupes/x.mov").write_bytes(b"SAME")
    (root / "dupes/y.mov").write_bytes(b"SAME")
    fp1 = fingerprint_file(root / "clips/a1.mov")
    fp2 = fingerprint_file(root / "dupes/x.mov")
    manifest = (
        "EXCHANGE version=1 timebase=24/1\n"
        f"  ASSET A1 path=clips/a1.mov size=10 fingerprint={fp1}\n"
        "    ALIAS name=mid target=A1\n"
        "  END\n"
        f"  ASSET A2 path=dupes/x.mov size=4 fingerprint={fp2}\n"
        "  END\nEND\n")
    doc = import_manifest_text(manifest)
    amap = doc.asset_map()
    resolver = AssetResolver(root)
    resolver.build_index()
    assert resolver.locate("mid", amap).status == "located"
    assert resolver.locate("A2", amap).status == "ambiguous"


# ---------- 端到端：验收 7 项指标与退出码 ----------

@pytest.fixture()
def built(tmp_path):
    decision, manifest, media = generate_fixture(tmp_path / "fx")
    return tmp_path / "fx"


def test_full_pipeline_acceptance_metrics(built):
    root = built
    decision = import_decision_text((root / "decision.txt").read_text("utf-8"))
    manifest = import_manifest_text((root / "manifest.txt").read_text("utf-8"))
    report = reconcile(manifest, decision, root / "media")
    assert report.entry_count == 12
    assert report.located_count == 9
    assert report.missing_count == 2
    assert report.ambiguous_count == 1
    assert report.drift_total == 3
    assert report.drift_max == 2
    assert report.max_depth == 3
    assert report.has_error is True
    # 重叠与时间基告警
    codes = {i.code for i in report.issues}
    assert {"overlap", "timebase_mismatch", "asset_missing", "asset_ambiguous"} <= codes


def test_cli_exit_codes_and_verify(built, capsys):
    root = built
    report_path = root / "out.json"
    rc = cli_main(["reconcile", "--decision", str(root / "decision.txt"),
                   "--manifest", str(root / "manifest.txt"),
                   "--root", str(root / "media"),
                   "--json-out", str(report_path)])
    assert rc == ExitCode.BUSINESS_ERROR
    assert verify_report(load_report_dict(report_path))[0] is True
    rc_verify = cli_main(["verify", "--report", str(report_path)])
    assert rc_verify == ExitCode.BUSINESS_ERROR
    # 严格模式阻止写出
    strict_out = root / "strict.json"
    rc_strict = cli_main(["reconcile", "--decision", str(root / "decision.txt"),
                          "--manifest", str(root / "manifest.txt"),
                          "--root", str(root / "media"),
                          "--json-out", str(strict_out), "--strict"])
    assert rc_strict == ExitCode.BUSINESS_ERROR
    assert not strict_out.exists()


def test_cli_format_error_exit_code(built):
    rc = cli_main(["reconcile", "--decision", str(built / "nope.txt"),
                   "--manifest", str(built / "manifest.txt"),
                   "--root", str(built / "media"),
                   "--json-out", str(built / "x.json")])
    assert rc == ExitCode.FORMAT_ERROR


def test_report_is_deterministic(built):
    decision = import_decision_text((built / "decision.txt").read_text("utf-8"))
    manifest = import_manifest_text((built / "manifest.txt").read_text("utf-8"))
    r1 = reconcile(manifest, decision, built / "media")
    r2 = reconcile(manifest, decision, built / "media")
    from src.report import report_to_dict
    assert json.dumps(r1 and report_to_dict(r1), sort_keys=True) == \
           json.dumps(report_to_dict(r2), sort_keys=True)
