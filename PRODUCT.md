# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Go 1.27 modular monolith, server-rendered HTML, plain CSS and JavaScript, PostgreSQL, Nginx, and FreeBSD amd64.

## Users

- Company staff who monitor public procurement notices without repeatedly searching 나라장터.
- Tenant administrators who configure filters, schedules, members, and reports.
- Platform administrators who manage tenants and collection health.

## Product Purpose

Collect public notices once, match each tenant's practical rules, and provide a concise scheduled HTML report without manual searching.

## Positioning

The product turns one shared public-data collection stream into tenant-specific, explainable matches. Every match shows the rule that selected it.

## Operating Context

The first deployment runs on an existing FreeBSD virtual server inside the company network. Users access it through a browser. The initial pilot uses one tenant but preserves SaaS tenant boundaries.

## Capabilities and Constraints

- Monitor construction, service, goods, and foreign-procurement notices.
- Collect hourly and perform a seven-day initial backfill.
- Support include, exclude, category, agency, region, amount, and deadline filters.
- Generate and retain one self-contained HTML report for each tenant and schedule window.
- Let authorized users create, list, and download reports through the web interface.
- Use only free and open-source software and the free public-data API.
- Do not use paid APIs, generative AI, frontend frameworks, Redis, or external queues.
- FreeBSD amd64 is the only officially supported server target for the pilot.

## Brand Commitments

- Product name: namo.
- Preserve the supplied reference image's navy, cyan, light-gray, and green palette; angled top rule; four-stage process; concise Korean copy; and restrained business tone.

## Evidence on Hand

- Visual reference: the user-provided NAMO interface screenshot.
- Official 나라장터 OpenAPI documentation and a user-approved implementation plan.
- No customer claims, production metrics, or live API credentials are present in the workspace.

## Product Principles

- Explain why each notice matched.
- Keep scheduled report generation automatic and observable.
- Prefer one deployable service and native browser capabilities.
- Separate local verification from live API, PostgreSQL, report-download, and FreeBSD verification.
- Preserve tenant isolation before adding sales automation.

## Accessibility & Inclusion

Support keyboard navigation, visible focus, semantic landmarks, Korean-language labels, responsive layouts, and WCAG AA contrast.
