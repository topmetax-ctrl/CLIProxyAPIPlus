#!/usr/bin/env python3
"""Extract protobuf-es field lists for named messages from the official
cursor-agent bundle. Read-only primary-source inspection.
"""
import re
import sys

path = sys.argv[1]
names = sys.argv[2:]
data = open(path, "r", errors="replace").read()

for name in names:
    marker = f'.typeName="agent.v1.{name}",'
    i = data.find(marker)
    if i == -1:
        print(f"### {name}: NOT FOUND")
        continue
    j = data.find("newFieldList((()=>[", i)
    if j == -1 or j - i > 400:
        print(f"### {name}: field list not found near typeName")
        continue
    start = j + len("newFieldList((()=>[")
    depth = 1
    k = start
    while k < len(data) and depth > 0:
        if data[k] == "[":
            depth += 1
        elif data[k] == "]":
            depth -= 1
        k += 1
    body = data[start:k - 1]

    print(f"### {name}")
    for m in re.finditer(r"\{no:(\d+),name:\"([a-z0-9_]+)\",kind:\"([a-z]+)\"([^}]*)\}", body):
        no, fname, kind, rest = m.groups()
        extra = []
        if "repeated:!0" in rest:
            extra.append("repeated")
        if "opt:!0" in rest:
            extra.append("optional")
        tm = re.search(r"T:([A-Za-z0-9_.$]+)", rest)
        if tm:
            extra.append("T=" + tm.group(1))
        om = re.search(r'oneof:"([a-z_]+)"', rest)
        if om:
            extra.append("oneof=" + om.group(1))
        print(f"  {no:>4}  {fname:<44} {kind:<8} {' '.join(extra)}")
    print()
