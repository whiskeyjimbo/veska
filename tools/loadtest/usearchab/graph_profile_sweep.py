#!/usr/bin/env python3
"""Graph the usearch build-profile sweep for the preset decision.

Reads one or more per-db sweep JSONs (emitted by TestProfileSweep) and draws:
  - LEFT: recall-vs-build-time Pareto frontier at the LARGEST corpus (where the
    speed/accuracy trade actually bites) - each (threads,ef) is a point, so the
    fast/balanced/accurate presets are literally the lower-right frontier.
  - RIGHT: recall-vs-N for a few key profiles, showing the ef64 recall decay that
    makes a wider beam (and thus a parallel preset to afford it) matter at scale.

    python3 graph_profile_sweep.py a.json b.json ... out.png
"""
import json
import sys
from collections import defaultdict

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

args = sys.argv[1:]
ins = [a for a in args if a.endswith(".json")] or ["/tmp/usearch-profile-sweep.json"]
OUT = next((a for a in args if a.endswith(".png")), "/tmp/usearch-profile-sweep.png")

rows, cores = [], "?"
for path in ins:
    d = json.load(open(path))
    rows.extend(d["rows"])
    cores = d.get("gomaxprocs", cores)

EF_COLOR = {64: "tab:red", 96: "tab:orange", 128: "tab:green", 192: "tab:blue", 256: "tab:purple"}
def mk(t):  # marker by thread count
    return "o" if t <= 1 else ("s" if t * 2 <= cores else "^")

fig, (axp, axn) = plt.subplots(1, 2, figsize=(16, 7))

# ---- LEFT: Pareto frontier at the largest N ----
Nmax = max(r["nodes"] for r in rows)
big = [r for r in rows if r["nodes"] == Nmax]
for r in big:
    c = EF_COLOR.get(r["ef"], "gray")
    axp.scatter(r["build_secs_med"], r["recall_min"], s=90, color=c, marker=mk(r["threads"]),
                edgecolor="black", linewidth=0.5, zorder=3)
    axp.annotate(r["profile"], (r["build_secs_med"], r["recall_min"]),
                 textcoords="offset points", xytext=(6, 4), fontsize=8)
# Pareto frontier (max recall for <= build time): lower build + higher recall dominates.
pts = sorted(big, key=lambda r: r["build_secs_med"])
frontier, best = [], -1
for r in pts:
    if r["recall_min"] > best:
        frontier.append(r); best = r["recall_min"]
axp.plot([r["build_secs_med"] for r in frontier], [r["recall_min"] for r in frontier],
         color="black", linestyle="--", alpha=0.4, zorder=2, label="Pareto frontier")
base = next((r for r in big if r["profile"] == "serial / ef64"), None)
if base:
    axp.axvline(base["build_secs_med"], color="gray", ls=":", alpha=0.6)
    axp.axhline(base["recall_min"], color="gray", ls=":", alpha=0.6)
    axp.annotate("today's default\n(serial/ef64)", (base["build_secs_med"], base["recall_min"]),
                 textcoords="offset points", xytext=(8, -24), fontsize=8, color="gray")
axp.set_title(f"Recall vs build time at N={Nmax:,} (the decision regime)\nlower-right = better; colour=ef, ○ serial △ parallel")
axp.set_xlabel("build time (s, median)")
axp.set_ylabel("recall (min over repeats - conservative)")
axp.grid(True, alpha=0.3)
axp.legend(fontsize=9)

# ---- RIGHT: recall decay vs N for key profiles ----
key = ["serial / ef64", "serial / ef128", "par x%d / ef128" % cores, "par x%d / ef64" % cores]
by = defaultdict(list)
for r in rows:
    by[r["profile"]].append(r)
for prof in key:
    pr = sorted(by.get(prof, []), key=lambda r: r["nodes"])
    if not pr:
        continue
    ef = pr[0]["ef"]; t = pr[0]["threads"]
    axn.plot([r["nodes"] for r in pr], [r["recall_med"] for r in pr],
             color=EF_COLOR.get(ef, "gray"), linestyle="-" if t <= 1 else ":",
             marker=mk(t), label=prof)
axn.set_title("Recall vs corpus size\n(ef64 decays as the graph grows - scale needs a wider beam)")
axn.set_xlabel("nodes (N)")
axn.set_ylabel("recall (median)")
axn.set_xscale("log")
axn.grid(True, which="both", alpha=0.3)
axn.legend(fontsize=9)

fig.suptitle(f"usearch build-profile calibration (GOMAXPROCS={cores}, real repos)", fontsize=13)
fig.tight_layout()
fig.savefig(OUT, dpi=120)
print(f"wrote {OUT}")
