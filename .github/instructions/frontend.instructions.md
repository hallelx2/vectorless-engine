---
applyTo: "**/*.ts,**/*.tsx,**/*.css"
---

TypeScript / Next.js review for this file. Cite `file:line` + the fix.

- Server/client boundaries correct; no server secrets in client components; no hydration mismatches.
- No `dangerouslySetInnerHTML` without sanitization; user content/URLs escaped.
- No `any` smuggled past the types; exhaustive handling of unions.
- Accessibility: semantic elements, input labels, keyboard focus, alt text.
- Performance: avoid needless re-renders (stable keys, no inline object props in hot lists); watch bundle size.
- Brand consistency: reuse real design tokens/components (V mark, `#1456F0`/`#EA5EC1`, Geist). Never invent a logo/color/font.
