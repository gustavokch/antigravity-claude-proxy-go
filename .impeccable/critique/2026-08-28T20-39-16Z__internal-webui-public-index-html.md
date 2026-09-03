---
target: internal/webui/public/index.html
total_score: 26
max_score: 40
na_heuristics: 
p0_count: 0
p1_count: 2
timestamp: 2026-08-28T20-39-16Z
slug: internal-webui-public-index-html
---
### Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Good live indicators & toast system; missing progress on async imports |
| 2 | Match System / Real World | 3 | Heavy technical jargon (CCR, TPS, Clamped Tokens) with no contextual hints |
| 3 | User Control and Freedom | 3 | Confirmation modals for deletes; lacks undo on setting toggles |
| 4 | Consistency and Standards | 2 | Divergent styling between Google vs Claude tabs & fragmented color tokens |
| 5 | Error Prevention | 3 | Good threshold boundaries; inline credential inputs lack pattern validation |
| 6 | Recognition Rather Than Recall | 3 | Email redaction tooltips work well; model override settings buried in modal |
| 7 | Flexibility and Efficiency | 2 | No global keyboard shortcuts; lacks batch/bulk account operations |
| 8 | Aesthetic and Minimalist Design | 2 | High visual noise, multiple conflicting neon accents & saturated badges |
| 9 | Error Recovery | 3 | Direct appeal links for banned accounts; connection errors lack recovery steps |
| 10 | Help and Documentation | 2 | Contextual tooltips missing for complex proxy engine configurations |
| **Total** | | **26/40** | **Acceptable (65%)** |

---

### Design Specificity Verdict

**LLM Assessment**: The interface has strong domain foundation for proxy management (multi-provider routing, live quotas, token metrics), but suffers from "cyberpunk dashboard syndrome" — heavy black backgrounds (`zinc-950`, `space-900`) layered with 6+ competing accent colors (cyan, green, purple, yellow, orange, amber, red). The typography hierarchy is overly fragmented across monospace and sans-serif fonts.

**Deterministic Scan**: 7 automated findings (`gray-on-color`) identified in `settings.html` (lines 380, 454, 532, 602, 672, 812, 1678) where muted `text-gray-400` sits on `bg-red-500` danger elements, violating WCAG contrast ratios.

**Visual Overlays**: No live browser injection active.

---

### Overall Impression
Robust technical capabilities and dense operational tooling, but visual fatigue and high cognitive load caused by neon visual clutter and fragmented UI patterns across views.

---

### What's Working
1. **Clear Provider Partitioning**: Clean visual separation between Google Cloud Code accounts and Claude Code accounts.
2. **Actionable State Badges**: Direct "APPEAL" and "FIX" actions inline for banned or degraded credentials.
3. **Responsive Metrics Grids**: Real-time quota meters and health indicators update cleanly.

---

### Priority Issues

- **[P1] Visual Noise & Competing Neon Accents**: Over-saturated palette (neon purple, cyan, emerald, yellow, orange) creates high cognitive load.
  - *Why it matters*: Users struggle to identify primary status indicators when every card glows.
  - *Fix*: Unify around a restrained semantic palette (primary blue/indigo, neutral grays, semantic green/red only for status).
  - *Suggested command*: `/impeccable quieter`

- **[P1] Inconsistent Multi-Provider & View Layouts**: Account management, Settings tabs, and Dashboard cards use mismatched spacing and interaction styles.
  - *Why it matters*: Increases extraneous mental load across page transitions.
  - *Fix*: Standardize card headers, filter controls, and tab bar patterns into reusable component templates.
  - *Suggested command*: `/impeccable layout`

- **[P2] Inaccessible Contrast on Danger / Reset Buttons**: `gray-on-color` contrast failures flagged in `settings.html`.
  - *Why it matters*: Fails WCAG AA standards, making destructive action buttons unreadable.
  - *Fix*: Replace `text-gray-400` with high-contrast `text-white` or dark foreground on colored backgrounds.
  - *Suggested command*: `/impeccable polish`

- **[P2] Lack of Power User Accelerators & Batch Controls**: No keyboard navigation for searches/modals and no bulk account selection.
  - *Why it matters*: Slows down operators managing 10+ accounts.
  - *Fix*: Add shortcut `/` to search, `Esc` modal management, and batch enable/disable toggles.
  - *Suggested command*: `/impeccable adapt`

- **[P2] Cryptic Technical Jargon Without Tooltips**: Acronyms like "CCR", "TPS", "Thinking Tokens Clamped" lack explanations.
  - *Why it matters*: Confuses first-time proxy operators.
  - *Fix*: Add subtle informational hover tooltips explaining metrics and engine settings.
  - *Suggested command*: `/impeccable clarify`

---

### Persona Red Flags

- **Alex (Power User)**: No keyboard shortcuts to switch tabs or filter models. Must click individually to toggle 10+ accounts.
- **Jordan (First-Timer)**: Confronted with dense proxy jargon ("CCR Retrievals", "Hybrid Token Bucket") on Dashboard with zero explanation.
- **Sam (Accessibility)**: Low contrast gray text on red buttons in Settings; custom range sliders lack complete keyboard ARIA states.
- **Riley (Stress Tester)**: Rapidly toggling accounts lacks optimistic UI locks; bulk imports have no rollback.
- **Casey (Mobile)**: Fixed sidebar on small viewports overflows horizontal cards in statistics grids.

---

### Minor Observations
- GitHub link in footer is hardcoded to upstream repo rather than current fork configuration.
- Toast notifications stack infinitely if multiple background sync failures occur.
- Settings view has duplicate save buttons across collapsible sections.

---

### Questions to Consider
- What if the dashboard used a single unified accent color and reserved bright neon tones strictly for alerts?
- Could account management support 1-click batch import and bulk enable/disable?
