#!/usr/bin/env python3
"""Compares perfsmoke results: lines "<base|head> <workload> <ns per op>".
Prints the fastest round per side and the head/base ratio per workload, writes the
same table to the GitHub job summary when there is one, and exits 1 when a
ratio is above the threshold, unless waived. Above 1.15 it only warns."""
import os
import sys

path, threshold, waived = sys.argv[1], float(sys.argv[2]), sys.argv[3] == "1"
samples = {}
for line in open(path):
    side, workload, ns = line.split()
    samples.setdefault(workload, {}).setdefault(side, []).append(int(ns))

rows, worst, bad = [], 0.0, []
for workload in sorted(samples):
    s = samples[workload]
    if "base" not in s or "head" not in s:
        sys.exit(f"perf smoke: {workload} is missing on one side")
    # The fastest round of each side: noise on a shared machine only adds
    # time, so the minimum is the steadiest estimate of what the code costs.
    b, h = min(s["base"]), min(s["head"])
    ratio = h / b
    worst = max(worst, ratio)
    rows.append((workload, b / 1e6, h / 1e6, ratio, len(s["head"])))
    if ratio > threshold:
        bad.append(workload)
    elif ratio > 1.15:
        print(f"::warning::perf smoke: {workload} is {ratio:.2f}x base")

table = ["| workload | base ms (min) | head ms (min) | head/base | rounds |", "|---|---|---|---|---|"]
table += [f"| {w} | {b:.1f} | {h:.1f} | {r:.3f} | {n} |" for w, b, h, r, n in rows]
print("\n".join(table))
summary = os.environ.get("GITHUB_STEP_SUMMARY")
if summary:
    with open(summary, "a") as f:
        f.write(f"### Perf smoke (threshold {threshold:.2f}, worst {worst:.3f})\n\n" + "\n".join(table) + "\n")
if bad:
    msg = f"perf smoke: {', '.join(bad)} slower than {threshold:.2f}x base"
    if waived:
        print(f"::warning::{msg} (waived by a perf-smoke: accept trailer)")
    else:
        print(f"::error::{msg}")
        sys.exit(1)
