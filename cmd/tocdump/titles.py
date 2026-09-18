#!/usr/bin/env python3
"""Compare the leaf titles of two tocdump runs.

    python cmd/tocdump/titles.py trees-after trees-jev

Reports, per document and overall, what share of the reference run's leaf
titles the candidate run also has (recall), and the reverse (precision),
after normalising case, punctuation and whitespace. Titles are compared
as sets; a title is matched if the normalised forms are equal or one is
a prefix of the other (contents entries are often wrapped or truncated).
"""
import json, re, sys
from pathlib import Path

def norm(t):
    t = t.lower()
    t = re.sub(r"[^a-z0-9 ]+", " ", t)
    return " ".join(t.split())

def leaves(ns):
    for n in ns:
        if n.get("nodes"):
            yield from leaves(n["nodes"])
        else:
            yield n

def titles(path):
    d = json.loads(Path(path).read_text())
    return [norm(n["title"]) for n in leaves(d.get("nodes") or [])], d

def matched(a, bs):
    for b in bs:
        if a == b or (len(a) >= 12 and (a.startswith(b) or b.startswith(a))):
            return True
    return False

def main(ref_dir, cand_dir):
    ref_dir, cand_dir = Path(ref_dir), Path(cand_dir)
    rows = []
    R = C = RM = CM = 0
    for rp in sorted(ref_dir.glob("*.json")):
        cp = cand_dir / rp.name
        if not cp.exists():
            continue
        rt, rd = titles(rp); ct, cd = titles(cp)
        if not rt or not ct:
            rows.append((rp.stem, len(rt), len(ct), None, None, cd.get("seconds", 0), cd.get("generative_calls", "?")))
            continue
        rm = sum(1 for t in rt if matched(t, ct)); cm = sum(1 for t in ct if matched(t, rt))
        R += len(rt); C += len(ct); RM += rm; CM += cm
        rows.append((rp.stem, len(rt), len(ct), rm / len(rt), cm / len(ct), cd.get("seconds", 0), cd.get("generative_calls", "?")))
    print(f"{'document':26} {'ref':>4} {'cand':>4} {'recall':>7} {'prec':>6} {'cand s':>7} gen")
    for name, r, c, rec, prec, sec, gen in rows:
        rec_s = f"{rec:.3f}" if rec is not None else "  n/a"
        prec_s = f"{prec:.3f}" if prec is not None else "  n/a"
        print(f"{name:26} {r:4d} {c:4d} {rec_s:>7} {prec_s:>6} {sec:7.1f} {gen}")
    if R and C:
        print(f"\noverall: recall {RM/R:.3f} ({RM}/{R})  precision {CM/C:.3f} ({CM}/{C})")

if __name__ == "__main__":
    main(*sys.argv[1:3])
