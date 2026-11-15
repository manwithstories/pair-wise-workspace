#!/usr/bin/env python3
import json
import pathlib
import subprocess
import sys
import tempfile

root = pathlib.Path(__file__).resolve().parent.parent
binary = root / "build" / "subtitle-audit"


def run(*args):
    return subprocess.run([str(binary), *map(str, args)], text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)


with tempfile.TemporaryDirectory(prefix="subtitle-audit-edge.") as temp:
    temp = pathlib.Path(temp)
    assert run("gen-fixtures", "--out", temp / "fixtures", "--count", "2",
               "--seed", "7").returncode == 0
    source = (temp / "fixtures" / "ep02_zh.ass").read_text()

    cycle = temp / "cycle.ass"
    cycle.write_text(source.replace(";Parent: season1=template", ";Parent: season1=ep02"))
    assert run("style-chain", "--script", cycle).returncode == 3

    missing_parent = temp / "missing_parent.ass"
    missing_parent.write_text(source.replace(";Parent: season1=template",
                                             ";Parent: season1=does_not_exist"))
    assert run("style-chain", "--script", missing_parent).returncode == 3

    invalid = temp / "invalid.ass"
    invalid.write_text("[Script Info]\nScriptType: v4.00+\n")
    assert run("tag-stats", "--script", invalid).returncode == 3

    missing_font = temp / "missing_font.ass"
    missing_font.write_text(source.replace(",Arial,", ",No Such Delivery Font,"))
    report = temp / "missing_font.json"
    result = run("render-probe", "--script", missing_font, "--frame", "1250",
                 "--out", report)
    assert result.returncode == 5
    assert json.loads(report.read_text())["missing_fonts"] == ["No Such Delivery Font"]

    unclosed = temp / "unclosed.ass"
    text = source
    start = text.index("{\\fs86")
    close = text.index("}", start)
    unclosed.write_text(text[:close] + text[close + 1:])
    tag_report = temp / "unclosed.json"
    assert run("tag-stats", "--script", unclosed, "--json", tag_report).returncode == 0
    assert json.loads(tag_report.read_text())["unclosed_braces"] > 0

    duplicate = temp / "duplicate.ass"
    lines = source.splitlines()
    style_index = next(i for i, line in enumerate(lines) if line.startswith("Style: "))
    lines.insert(style_index + 1, lines[style_index])
    duplicate.write_text("\n".join(lines) + "\n")
    result = run("style-chain", "--script", duplicate)
    assert result.returncode == 0 and "duplicate style" in result.stderr

print("edge-cases: ok")
