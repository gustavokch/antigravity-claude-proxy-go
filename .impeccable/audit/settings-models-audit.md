# Technical Quality Audit: Settings Models (`/#settings/models`)

Date: 2026-08-28  
Target: `internal/webui/public/views/settings.html` (Tab: Models Configuration), `internal/webui/public/js/components/models.js`

---

## Audit Health Score

| # | Dimension | Score | Key Finding |
|---|-----------|-------|-------------|
| 1 | Accessibility | 1/4 | Missing ARIA labels on action icon buttons and inputs; sub-4.5:1 contrast on muted text |
| 2 | Performance | 2/4 | O(N) array scans inside Alpine `x-for` rows during every render tick |
| 3 | Responsive Design | 2/4 | Sub-24px touch targets on action buttons; fixed column widths clip on mobile |
| 4 | Theming | 2/4 | Divergent theme tokens between OpenRouter (`neon-*`) and Claude/Kimi (`gray-*`/`emerald-*`) |
| 5 | Implementation Integrity | 1/4 | Invalid DOM nesting of Kimi section inside Model Mappings; 3 duplicate modal patterns |
| **Total** | | **8/20** | **Poor (major overhaul needed)** |

---

## Implementation Integrity Verdict

**FAIL**: Target implementation contains major structural defects, visual token divergence, and component duplication. Section 0b (Kimi Code Gateway) is incorrectly nested inside Section 2 (Model Mappings Card) DOM tree (`settings.html:1477`). Gateway sections use conflicting CSS token systems (`space-*`/`neon-*` vs `gray-800`/`emerald-500`). Three discovery modals duplicate identical UI workflows with inconsistent markup.

---

## Executive Summary

- **Audit Health Score**: 8/20 (Poor)
- **Total Issues Found**: 9 (P0: 1, P1: 3, P2: 3, P3: 2)
- **Top Critical Issues**:
  1. Invalid DOM hierarchy: Kimi Code Gateway card nested inside Model Aliases container card (`views/settings.html:1477`).
  2. Missing accessible names/ARIA attributes on all table action buttons, icon toggles, and modal controls (`WCAG 4.1.2`).
  3. Insufficient color contrast on `text-gray-500` / `text-gray-600` muted labels on dark ground (`WCAG 1.4.3`).
  4. Token fragmentation: Claude Code and Kimi gateway sections bypass design system tokens.

---

## Detailed Findings by Severity

### [P0] Invalid DOM Nesting of Kimi Gateway Card
- **Location**: `internal/webui/public/views/settings.html:1476-1550`
- **Category**: Implementation Integrity
- **Impact**: Breaks visual layout containment; places entire Kimi gateway form inside Model Aliases card border.
- **WCAG/Standard**: HTML5 Specification (DOM tree containment)
- **Recommendation**: Move Section 0b (`Kimi Code Gateway`) outside Section 2 card; align sequence with Section 0, 0b, 0c, 1, 2.
- **Suggested Command**: `/impeccable harden`

### [P1] Missing ARIA Labels on Interactive Icon Buttons
- **Location**: `internal/webui/public/views/settings.html:1095, 1102, 1172, 1275, 1330, 1408, 1416, 1537, 1650, 1660, 1678, 1891, 2065`
- **Category**: Accessibility
- **Impact**: Screen reader users hear "button" with no purpose or target context.
- **WCAG/Standard**: WCAG 2.1 AA §4.1.2 (Name, Role, Value), §1.3.1 (Info and Relationships)
- **Recommendation**: Add `aria-label` or `aria-labelledby` to all SVG-only and symbol buttons (`×`, `+`, `↑`, `↓`, `✕`). Add `aria-expanded` to collapsible row triggers.
- **Suggested Command**: `/impeccable harden`

### [P1] Missing Labels and Accessible Names on Form Controls
- **Location**: `internal/webui/public/views/settings.html:1073, 1085, 1264, 1266, 1283, 1326, 1328, 1337, 1533, 1542, 1953, 2027, 2141`
- **Category**: Accessibility
- **Impact**: Form inputs in tables lack associated label elements or `aria-label` bindings, blocking assistive tech navigation.
- **WCAG/Standard**: WCAG 2.1 AA §1.3.1, §4.1.2
- **Recommendation**: Add explicit `aria-label` attributes derived from column headers or model identifiers (e.g. `aria-label="Alias for " + item.id`).
- **Suggested Command**: `/impeccable harden`

### [P1] Low Contrast Text on Dark Ground
- **Location**: `internal/webui/public/views/settings.html:1018, 1082, 1126, 1191, 1251, 1600, 1703, 1915, 2033`
- **Category**: Accessibility
- **Impact**: Text rendered with `text-gray-500` / `text-gray-600` on `bg-space-800` / `bg-space-900` yields contrast ratios between 2.1:1 and 3.2:1 (below 4.5:1 minimum).
- **WCAG/Standard**: WCAG 2.1 AA §1.4.3 (Contrast Minimum)
- **Recommendation**: Upgrade secondary labels to `text-gray-400` or `text-space-dim` (minimum contrast ratio ≥ 4.5:1).
- **Suggested Command**: `/impeccable colorize`

### [P2] Inconsistent Design System Token Usage
- **Location**: `internal/webui/public/views/settings.html:1202-1344, 1476-1550`
- **Category**: Theming
- **Impact**: Claude Code and Kimi gateway sections use Tailwind default palette (`gray-800`, `gray-700`, `emerald-500`) instead of application tokens (`space-900`, `space-border`, `neon-purple`, `neon-cyan`), creating visible design seams.
- **Recommendation**: Replace generic Tailwind container and table classes with `card bg-space-900/30 border border-space-border/50` and `standard-table`.
- **Suggested Command**: `/impeccable layout`

### [P2] Touch Target Sizes Below 44x44px
- **Location**: `internal/webui/public/views/settings.html:1073, 1095, 1102, 1264, 1275, 1650, 1660, 1678`
- **Category**: Responsive Design
- **Impact**: Table action icons and toggles measure 20px–28px, causing misclicks on touch devices.
- **WCAG/Standard**: WCAG 2.1 AAA §2.5.5 (Target Size), WCAG 2.2 AA §2.5.8 (Target Size Minimum)
- **Recommendation**: Add padding wrappers or min-size constraints (`min-h-[36px] min-w-[36px]` with target expansion) to action buttons on mobile breakpoints.
- **Suggested Command**: `/impeccable adapt`

### [P2] Unmemoized O(N) Lookups in Table Render Loop
- **Location**: `internal/webui/public/views/settings.html:1563-1600`, `internal/webui/public/js/components/models.js:1341-1396`
- **Category**: Performance
- **Impact**: Every row evaluation in `allConfiguredModels` executes multiple array scans (`some()`, `includes()`, `isCustomAlias()`), causing lag with large model catalogs.
- **Recommendation**: Pre-index model categories and metadata maps into lookup dictionaries in data store before rendering.
- **Suggested Command**: `/impeccable optimize`

### [P3] Triplicated Model Discovery Modal Logic
- **Location**: `internal/webui/public/views/settings.html:1881-2210`, `internal/webui/public/js/components/models.js:690-827, 1011-1110`
- **Category**: Implementation Integrity
- **Impact**: OpenRouter, Kimi, and Claude Code discovery modals duplicate filter, search, selection, and table rendering logic.
- **Recommendation**: Extract generic model discovery modal component or template partial.
- **Suggested Command**: `/impeccable distill`

### [P3] Mobile Column Overflow in Gateway Tables
- **Location**: `internal/webui/public/views/settings.html:1056, 1249, 1312, 1370, 1553`
- **Category**: Responsive Design
- **Impact**: Multi-column tables without responsive column hiding compress text and break layout on narrow viewports (<640px).
- **Recommendation**: Add responsive utility classes (`hidden sm:table-cell`) for secondary columns (Context Length, Display Name, Capabilities) on narrow viewports.
- **Suggested Command**: `/impeccable adapt`

---

## Recommended Action Plan

1. **[P0/P1] `/impeccable harden`**: Fix Kimi section DOM nesting defect and add full ARIA label coverage to all table controls and form inputs.
2. **[P1/P2] `/impeccable layout`**: Standardize Claude Code and Kimi gateway cards and tables to use `standard-table` and `space-*`/`neon-*` design tokens.
3. **[P1] `/impeccable colorize`**: Fix contrast on muted text, badges, and toggle indicators to meet WCAG AA 4.5:1.
4. **[P2/P3] `/impeccable adapt`**: Increase touch target hit areas and add responsive column wrapping for mobile viewports.
5. **[P2] `/impeccable optimize`**: Index model lookup states in JS store to eliminate O(N) evaluations in table render loop.
6. **[P3] `/impeccable polish`**: Final quality pass across alignments, focus rings, and badge sizing.
