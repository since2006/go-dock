#!/usr/bin/env python3
"""Builds the subset fonts embedded in the firmware.

The screen renders text at runtime with LVGL's TinyTTF, so one font file
per weight covers every size. Sources are the Google Fonts variable fonts
(SIL OFL 1.1):

  https://github.com/google/fonts/tree/main/ofl/notosanssc
  https://github.com/google/fonts/tree/main/ofl/jetbrainsmono

Usage (needs `pip install fonttools`):

  python3 make_fonts.py "NotoSansSC[wght].ttf" "JetBrainsMono[wght].ttf" ../main/fonts
"""
import os
import sys

from fontTools import subset
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer

ASCII = "".join(chr(c) for c in range(0x20, 0x7F))
# Punctuation and symbols the UI or typical task text uses.
EXTRA = "，。、；：？！“”‘’（）《》【】「」『』—…·～￥％　°℃×÷±←→↑↓↻●○■□▲△★☆√"


def gb2312(last_row):
    """GB2312 hanzi: level 1 is rows 0xB0-0xD7 (3755 common characters),
    level 2 continues to 0xF7 (3008 more)."""
    out = []
    for hi in range(0xB0, last_row + 1):
        for lo in range(0xA1, 0xFF):
            try:
                out.append(bytes([hi, lo]).decode("gb2312"))
            except UnicodeDecodeError:
                pass
    return "".join(out)


def build(src, weight, text, dst):
    font = TTFont(src)
    opts = subset.Options()
    opts.hinting = False
    opts.layout_features = []  # TinyTTF uses neither GSUB nor GPOS
    opts.drop_tables += ["GSUB", "GPOS", "GDEF", "STAT", "DSIG", "MVAR", "HVAR"]
    opts.notdef_outline = True
    opts.name_IDs = ["*"]
    sub = subset.Subsetter(opts)
    sub.populate(text=text)
    sub.subset(font)
    static = instancer.instantiateVariableFont(font, {"wght": weight})
    static.save(dst)
    print(f"{os.path.basename(dst)}: {len(text)} chars, {os.path.getsize(dst) // 1024} KiB")


def main():
    noto, mono, outdir = sys.argv[1:4]
    os.makedirs(outdir, exist_ok=True)
    common = gb2312(0xD7)
    full = gb2312(0xF7)
    # Regular covers all of GB2312 for task text; Bold only the common
    # level and falls back to Regular at runtime.
    build(noto, 400, ASCII + EXTRA + full, os.path.join(outdir, "NotoSansSC-Regular.ttf"))
    build(noto, 700, ASCII + EXTRA + common, os.path.join(outdir, "NotoSansSC-Bold.ttf"))
    build(mono, 500, ASCII, os.path.join(outdir, "JetBrainsMono-Medium.ttf"))
    build(mono, 700, ASCII, os.path.join(outdir, "JetBrainsMono-Bold.ttf"))


if __name__ == "__main__":
    main()
