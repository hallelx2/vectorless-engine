# Images

Updated 2026-10-11.

| File | Status |
|---|---|
| `how-it-works.svg` | Current. The pipeline since 2026-10-06: contents found and placed by Jev, judgewalk, verbatim evidence pages, pincited answer. |
| `vs-rag.svg` | Current (right-hand panel updated 2026-10-11). |
| `banner.png`, `screenshot-dashboard.png`, `screenshot-install.png` | Raster images, not redrawn. |
| `architecture.svg`, `hero.svg`, `ingest-pipeline.svg`, `retrieval-comparison.svg`, `chunked-reasoning.svg` | **Out of date.** They show the original design: a chat model summarises every section at ingest (`pending → parsing → summarizing → ready`) and navigates those summaries with `single-pass` or `chunked-tree` at query time. Production stopped working that way in September 2026. See the [README](../../README.md#how-it-works) and the [evaluation reports](../evaluations/README.md). |
| `roadmap.svg` | **Out of date.** The project is no longer actively developed. |

None of the out-of-date images is linked from the README.
