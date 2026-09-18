"""Evidence-page coverage: does the TOC tree put the answer where it is?

    python cmd/tocdump/coverage.py <trees-dir> [<trees-dir-2> ...]

FinanceBench gives every question a gold evidence page. tocdump gives every
document a section tree with page ranges. This joins them and asks, per
question: is the gold page inside some leaf section, and how big is that
leaf? A tree that covers every gold page in a 3-page leaf is doing its job;
one that covers it in a 140-page leaf has found the document, not the
answer.

This is the accuracy gate for every context cut in HAL-1366. "The model
agreed with another model" is not a check — the other model was
rate-limited and silently empty on 13 of 21 documents. Gold pages are.

Two directories compare before/after in one table. Coverage that drops is
a cut that gets reverted, whatever it saved.
"""

from __future__ import annotations

import json
import statistics
import sys
from pathlib import Path


def leaves(nodes, out):
    for n in nodes or []:
        if n.get("nodes"):
            leaves(n["nodes"], out)
        else:
            out.append(n)
    return out


def load_trees(d: Path) -> dict:
    trees = {}
    for f in sorted(d.glob("*.json")):
        t = json.loads(f.read_text())
        trees[t["doc"]] = t
    return trees


def gold():
    from datasets import load_dataset
    ds = load_dataset("PatronusAI/financebench", split="train")
    out = []
    for r in ds:
        for ev in r.get("evidence") or []:
            pg = ev.get("evidence_page_num")
            if pg is None:
                continue
            # FinanceBench pages are 0-indexed; the tree is 1-indexed.
            out.append((r["doc_name"], int(pg) + 1, r["financebench_id"]))
    return out


def score(trees: dict, gold_pages) -> dict:
    covered = hit_spans = 0
    seen = uncovered_docs = 0
    spans = []
    missing_doc = set()
    for doc, page, _qid in gold_pages:
        t = trees.get(doc)
        if not t or t.get("err") or not t.get("nodes"):
            missing_doc.add(doc)
            continue
        seen += 1
        # A leaf covers the page if page in [start, end]; end==0 means open.
        best = None
        for lf in leaves(t["nodes"], []):
            s, e = lf.get("start_page", 0), lf.get("end_page", 0)
            if s <= 0:
                continue
            if e <= 0:
                e = t.get("pages", s)
            if s <= page <= e:
                span = e - s + 1
                if best is None or span < best:
                    best = span
        if best is not None:
            covered += 1
            spans.append(best)
    return {
        "questions_with_tree": seen,
        "covered": covered,
        "coverage": covered / seen if seen else 0.0,
        "median_leaf_span": statistics.median(spans) if spans else None,
        "docs_without_tree": sorted(missing_doc),
        "docs_total": len(trees),
        "docs_ok": sum(1 for t in trees.values() if t.get("nodes") and not t.get("err")),
        "build_seconds": sum(t.get("seconds", 0) for t in trees.values()),
        "build_cost": sum(t.get("cost_usd", 0) for t in trees.values()),
        "build_requests": sum(t.get("requests", 0) for t in trees.values()),
    }


def main():
    dirs = [Path(a) for a in sys.argv[1:]]
    if not dirs:
        print(__doc__)
        return 1
    g = gold()
    print(f"gold evidence pages: {len(g)} across {len({d for d,_,_ in g})} documents\n")

    rows = [(d.name, score(load_trees(d), g)) for d in dirs]
    w = max(len(n) for n, _ in rows) + 2
    print(f"{'run':<{w}} {'docs ok':>8} {'q seen':>7} {'covered':>8} {'coverage':>9} {'median span':>12} {'build s':>9} {'req':>5} {'cost':>9}")
    print("-" * (w + 74))
    for name, s in rows:
        ms = f"{s['median_leaf_span']:.0f}p" if s["median_leaf_span"] is not None else "—"
        print(f"{name:<{w}} {s['docs_ok']:>3}/{s['docs_total']:<4} {s['questions_with_tree']:>7} {s['covered']:>8} "
              f"{s['coverage']:>9.3f} {ms:>12} {s['build_seconds']:>9.0f} {s['build_requests']:>5} ${s['build_cost']:>8.4f}")
    for name, s in rows:
        if s["docs_without_tree"]:
            print(f"\n{name}: no usable tree for {len(s['docs_without_tree'])} doc(s): {', '.join(s['docs_without_tree'][:8])}")
    print("\ncoverage = share of gold pages inside some leaf; median span = size of the")
    print("tightest leaf that covered them. Both matter: a whole-document leaf covers")
    print("everything and locates nothing.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
