#!/usr/bin/env python3
"""Generate deterministic fixtures: two TTFs, a mixed-language corpus,
and an expected-summary subset (fixtures/e.json)."""
import argparse
import json
import os
import random

from fontTools.fontBuilder import FontBuilder
from fontTools.pens.ttGlyphPen import TTGlyphPen

UPEM = 1000
ASCENT = 800
DESCENT = -200


def adv_for(cp):
    if cp in (0x0301, 0x3099):
        return 0
    if cp == 0x20:
        return 300
    if cp < 0x7F:
        ch = chr(cp)
        if ch.isalnum():
            return 600
        return 400
    if 0x0600 <= cp <= 0x06FF or 0x0E00 <= cp <= 0x0E7F:
        return 500
    return 1000  # CJK / kana / emoji / fullwidth


def build_font(path, cps, family):
    cps = sorted(set(cps))
    order = [".notdef"] + ["u%04X" % cp for cp in cps]
    fb = FontBuilder(UPEM, isTTF=True)
    fb.setupGlyphOrder(order)
    fb.setupCharacterMap({cp: "u%04X" % cp for cp in cps})
    glyphs = {}
    metrics = {}
    for name in order:
        pen = TTGlyphPen(None)
        if name == ".notdef":
            adv = 0
        else:
            cp = int(name[1:], 16)
            adv = adv_for(cp)
            if adv > 0:
                w = int(adv * 0.8)
                pen.moveTo((0, 0))
                pen.lineTo((w, 0))
                pen.lineTo((w, 700))
                pen.lineTo((0, 700))
                pen.closePath()
        glyphs[name] = pen.glyph()
        metrics[name] = (adv, 0)
    fb.setupGlyf(glyphs)
    fb.setupHorizontalMetrics(metrics)
    fb.setupHorizontalHeader(ascent=ASCENT, descent=DESCENT)
    fb.setupMaxp()
    fb.setupNameTable({
        "familyName": family, "styleName": "Regular",
        "uniqueFontIdentifier": family, "fullName": family,
        "psName": family, "version": "1.0"})
    fb.setupOS2(sTypoAscender=ASCENT, sTypoDescender=DESCENT,
                usWinAscent=ASCENT, usWinDescent=-DESCENT)
    fb.setupPost()
    fb.font["head"].created = 0
    fb.font["head"].modified = 0
    fb.save(path)


JA = ["こんにちは", "世界", "字幕", "テスト", "品質", "確認", "東京", "映画"]
ZH = ["你好", "世界", "字幕", "本地化", "质量", "检查", "谢谢", "电影"]
EN = ["the", "quick", "brown", "fox", "subtitle", "layout", "measure",
      "export", "scene", "dialog", "movie", "line"]
AR = ["مرحبا", "بالعالم", "ترجمة", "فيلم", "جودة"]
TH = ["สวัสดี", "โลก", "ภาพยนตร์", "วัด"]
EMOJI = ["😀", "🎬", "👍", "👨\u200d👩\u200d👧", "❤️"]
PUNCT = "。「」！？：；（）【】…"


def ts(ms):
    h, ms = divmod(ms, 3600000)
    m, ms = divmod(ms, 60000)
    s, ms = divmod(ms, 1000)
    return "%02d:%02d:%02d,%03d" % (h, m, s, ms)


def cue_block(cid, lang, text, speaker=""):
    start = (int(cid[1:]) - 1) * 2000
    lines = ["=== cue", "id: %s" % cid, "lang: %s" % lang]
    if speaker:
        lines.append("speaker: %s" % speaker)
    lines.append("time: %s --> %s" % (ts(start), ts(start + 1800)))
    lines.append("text: %s" % text)
    return "\n".join(lines)


def random_text(rng):
    parts = []  # (text, approx advance units)
    target = rng.randint(15000, 55000)  # approx. advance units
    used = 0
    pools = [JA, ZH, EN, EN, AR, TH]
    while used < target:
        pool = rng.choice(pools)
        w = rng.choice(pool)
        u = sum(adv_for(ord(c)) for c in w)
        if rng.random() < 0.10:
            em = rng.choice(EMOJI)
            w += em
            u += sum(adv_for(ord(c)) for c in em)
        if rng.random() < 0.12:
            tag = rng.choice("biu")
            w = "<%s>%s</%s>" % (tag, w, tag)
        parts.append((w, u))
        used += u + 300
    sep = " " if rng.random() < 0.7 else ""
    text = sep.join(p for p, _ in parts)
    if rng.random() < 0.15 and sep == " ":
        # pick a split space so each forced segment still fits one line
        pref = 0
        cands = []
        for i, (_, u) in enumerate(parts[:-1]):
            pref += u + 300
            if pref <= 32000 and used - pref <= 32000:
                cands.append(i)
        if cands:
            idx = rng.choice(cands)
            text = (sep.join(p for p, _ in parts[:idx + 1]) + "\\n" +
                    sep.join(p for p, _ in parts[idx + 1:]))
    if rng.random() < 0.08:
        text += " ✅"
    if rng.random() < 0.08:
        text = "é" + text
    return text


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--cues", type=int, default=240)
    ap.add_argument("--seed", type=int, default=10261)
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)
    rng = random.Random(args.seed)

    # ---- corpus ----
    url_long = "https://example.com/" + "a" * 90
    blocks = []

    def add(cid, lang, text, speaker=""):
        blocks.append(cue_block(cid, lang, text, speaker))

    add("c0001", "en", "AAAA")
    add("c0002", "en", "AAAA\\nBBBB")
    add("c0003", "ja", "あいうえお")
    add("c0004", "en", "check %s end" % url_long)          # overflow: width
    add("c0005", "en", "rare ཅ glyph")                     # missing glyph
    add("c0006", "ar", "مرحبا بالعالم")
    add("c0007", "en", "cast 👨\u200d👩\u200d👧 🎬😀")
    add("c0008", "ja", "éカ\u3099")
    add("c0009", "en", "see https://example.org/" + "b" * 80)
    add("c0010", "zh", "链接 https://example.net/" + "c" * 80 + " 在这里")
    add("c0011", "ja", "一\\n二\\n三\\n四\\n五")            # overflow: max_lines
    add("c0012", "en", "one\\ntwo\\nthree\\nfour\\nfive")
    add("c0013", "zh", "甲\\n乙\\n丙\\n丁\\n戊")
    add("c0014", "th", "สวัสดี ཅ โลก")                     # missing glyph
    add("c0015", "ar", "مرحبا ཅ بالعالم")                  # missing glyph
    for i in range(16, args.cues + 1):
        lang = rng.choice(["ja", "zh", "en", "ar", "th"])
        add("c%04d" % i, lang, random_text(rng),
            speaker="speaker%d" % (i % 5) if i % 3 == 0 else "")
    corpus = "\n\n".join(blocks) + "\n"
    with open(os.path.join(args.out, "c.txt"), "w", encoding="utf-8") as f:
        f.write(corpus)

    # ---- fonts: coverage derived from the corpus ----
    a_cps, b_cps = set(), set()
    for ch in corpus:
        cp = ord(ch)
        if cp < 0x20:
            continue
        if (0x0600 <= cp <= 0x06FF) or (0x0E00 <= cp <= 0x0E7F) or cp > 0xFFFF:
            b_cps.add(cp)
        elif cp == 0x0F45:
            continue  # deliberately uncovered -> missing_glyph
        else:
            a_cps.add(cp)
    a_cps |= {ord(c) for c in PUNCT} | {0x0301, 0x3099}
    b_cps |= {0x1F600, 0x1F3AC, 0x1F44D, 0x1F468, 0x1F469, 0x1F467, 0x2764}
    build_font(os.path.join(args.out, "a.ttf"), a_cps, "sublay-a")
    build_font(os.path.join(args.out, "b.ttf"), b_cps, "sublay-b")

    # ---- expected summary subset ----
    def w266(units):
        return (units * (48 << 6) * 2 + UPEM) // (2 * UPEM)

    golden = [
        {"cue_id": "c0001", "line_count": 1, "breaks": [],
         "width": 4 * w266(600), "overflow": False, "missing_glyph": False},
        {"cue_id": "c0002", "line_count": 2, "breaks": [4],
         "width": None, "overflow": False, "missing_glyph": False},
        {"cue_id": "c0003", "line_count": 1, "breaks": [],
         "width": 5 * w266(1000), "overflow": False, "missing_glyph": False},
        {"cue_id": "c0004", "line_count": None, "breaks": None,
         "width": None, "overflow": True, "missing_glyph": False},
        {"cue_id": "c0005", "line_count": None, "breaks": None,
         "width": None, "overflow": False, "missing_glyph": True},
        {"cue_id": "c0006", "line_count": 1, "breaks": [],
         "width": None, "overflow": False, "missing_glyph": False},
        {"cue_id": "c0007", "line_count": 1, "breaks": [],
         "width": None, "overflow": False, "missing_glyph": False},
        {"cue_id": "c0008", "line_count": 1, "breaks": [],
         "width": w266(1600), "overflow": False, "missing_glyph": False},
    ]
    expected = {"cues": args.cues, "overflow_count": 6, "missing_glyph": 3,
                "golden": golden}
    with open(os.path.join(args.out, "e.json"), "w", encoding="utf-8") as f:
        json.dump(expected, f, ensure_ascii=False, indent=1, sort_keys=True)
        f.write("\n")
    print("fixtures: cues=%d a_glyphs=%d b_glyphs=%d" %
          (args.cues, len(a_cps), len(b_cps)))


if __name__ == "__main__":
    main()
