# Design System

## Direction

A calm, compact operations console for technical administrators. Prioritize accurate status, clear hierarchy, safe actions, and high information density without visual clutter.

## Theme

- Support light and dark color schemes through the existing CSS variables.
- Use restrained neutral surfaces with blue for primary actions, green for healthy states, orange for warnings, and red only for destructive or blocked states.
- Preserve the existing translucent card treatment, rounded corners, and subtle shadows.

## Color Palette

- Background: `#f5f5f7`
- Surface: `rgba(255,255,255,0.72)`
- Solid surface: `#ffffff`
- Primary text: `#1d1d1f`
- Secondary text: `#86868b`
- Accent: `#0071e3`
- Success: `#34c759`
- Warning: `#ff9f0a`
- Danger: `#ff3b30`
- Divider: `rgba(0,0,0,0.06)`

Dark-mode values should continue to come from the existing token overrides rather than component-specific hard-coded colors.

## Typography

- Primary family: Inter
- CJK fallback: Noto Sans SC
- System sans-serif fallback for unavailable web fonts
- Use strong but compact page titles, medium-weight section headings, and tabular or monospace treatment where IP addresses, CIDR prefixes, identifiers, and numeric usage data benefit from alignment.

## Layout

- Keep the persistent sidebar and existing content-width conventions.
- Organize administrative pages into distinct task-oriented sections rather than one undifferentiated table.
- Use cards for related controls and results, with consistent 14px outer radii and 10px control radii.
- On narrow screens, stack toolbars and forms, allow tables to scroll horizontally, and keep primary actions reachable without forcing viewport-wide fixed widths.

## Components

### Page Headers

Combine a concise title and explanatory subtitle with refresh or primary actions aligned to the opposite edge. Actions wrap below the title on narrow screens.

### Cards

Use a clear card header, optional supporting description, and a body with predictable spacing. Avoid unnecessary decorative cards or nested cards.

### Tables

- Use descriptive column headings and accessible row actions.
- Provide loading, empty, filtered-empty, and error states inside the table region.
- Keep identifiers readable and expose the rule responsible for derived states such as CIDR blocking.
- Provide search, status filters, and sorting when lists can grow.

### Forms

- Every field has a visible label or accessible name.
- Placeholder text is illustrative, not the only instruction.
- Validation and risk messages appear near the action.
- Disable submit controls while requests are pending and report completion through the existing notice system.

### Status

Use text and shape in addition to color. Distinguish active, blocked, resolving, resolved, and failed states. When an address is blocked by a CIDR, show the matched prefix rather than only a generic blocked badge.

### Modals

Use the existing modal system for confirmations. Destructive actions identify the target. Rules that include the current administrator address or cover a broad network require a stronger warning and explicit acknowledgement before submission.

## Motion

- Keep transitions short and functional.
- Respect `prefers-reduced-motion` where existing styles support it.
- Do not use motion as the sole indicator of progress or state.

## Accessibility

- Preserve visible keyboard focus.
- Use semantic buttons, labels, headings, tables, and live regions.
- Ensure icon-only controls have accessible names.
- Maintain sufficient text and status contrast in both themes.
- Do not rely on hover or color alone.

## Content Style

- Use direct operational language.
- Describe consequences before risky actions.
- Keep English and Chinese translations equivalent in meaning, including loading, empty, error, success, filtering, sorting, and CIDR risk messages.
- Avoid vague labels such as “OK” when a specific action such as “Remove rule” is clearer.
