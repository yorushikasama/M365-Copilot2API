# Product Context

## Product

M365 Copilot2API is a self-hosted Go gateway that translates Microsoft 365 Copilot ChatHub traffic into OpenAI- and Anthropic-compatible APIs. It includes an embedded web administration console for operating accounts, credentials, conversations, usage, proxies, models, settings, and network access controls.

## Target Users

- Developers and technical operators who self-host the gateway
- Administrators responsible for account availability, API access, traffic, and security
- Teams using OpenAI- or Anthropic-compatible clients with Microsoft 365 Copilot subscriptions

## Core Purpose

Make a technically complex compatibility gateway observable, controllable, and safe to operate without requiring administrators to edit local data files or inspect internal protocols manually.

## Brand Personality

- Technical and trustworthy
- Calm rather than promotional
- Direct, compact, and operational
- Safety-conscious without creating unnecessary alarm
- Familiar to developers while remaining understandable to occasional administrators

## Product Principles

1. **Operational truth first.** Status, rule matches, request volume, and errors must reflect backend behavior precisely.
2. **Safe administration.** Destructive or access-affecting actions must explain their consequences and guard against accidental lockout.
3. **Progressive detail.** Show essential state and actions immediately, with diagnostic detail available when needed.
4. **Self-hosted resilience.** Interfaces must remain useful with partial data, failed external lookups, slow networks, and empty installations.
5. **Consistent compatibility.** New API fields and interface behavior should preserve existing clients and deployment conventions.
6. **Accessible across devices and languages.** Administrative workflows must support keyboard use, assistive technology, responsive layouts, and the console's supported locales.

## Primary Experience Goals

- Operators can understand gateway health and usage at a glance.
- Security rules are easy to create, inspect, search, filter, and remove.
- Traffic observations remain distinct from enforcement configuration.
- Every asynchronous action has clear loading, empty, success, and error feedback.
- Risky CIDR rules and self-blocking actions require explicit, contextual acknowledgement.

## Anti-References

Avoid interfaces that are:

- Decorative dashboards with weak information hierarchy
- Dense raw-data tables without filtering or responsive behavior
- Ambiguous about whether an IP is directly blocked or matched by a CIDR rule
- Dependent on native browser confirmation dialogs
- Silent during loading, lookup failures, or successful mutations
- Overly destructive, especially when modifying embedded resources or user configuration

## Technical Context

- Backend: Go
- Frontend: framework-free embedded HTML, CSS, and JavaScript
- Runtime authority: files embedded from `internal/web/web` by `internal/web/security_http.go`
- Mirrored editable web resources: `web`
- Localization: in-page translation dictionary and DOM text translation
- Distribution: self-contained server binary with embedded static assets
