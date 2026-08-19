#!/usr/bin/env python3
"""Extract context windows around identifiers in the official cursor-agent bundle.

Read-only primary-source inspection: no bundle is modified or executed.
"""
import sys

path = sys.argv[1]
width = int(sys.argv[-1]) if sys.argv[-1].isdigit() else 400
kws = [a for a in sys.argv[2:] if not a.isdigit()]

data = open(path, "r", errors="replace").read()
for kw in kws:
    print("=" * 20, kw, "=" * 20)
    start = 0
    hits = 0
    while hits < 6:
        i = data.find(kw, start)
        if i == -1:
            break
        hits += 1
        lo = max(0, i - width)
        hi = min(len(data), i + width)
        print(f"--- hit {hits} @ offset {i} ---")
        print(data[lo:hi].replace("\n", "\\n"))
        print()
        start = i + len(kw)
    if hits == 0:
        print("(no hits)")
