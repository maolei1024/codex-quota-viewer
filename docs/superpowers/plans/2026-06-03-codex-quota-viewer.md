# Codex Quota Viewer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an independent read-only Dockerized web dashboard for Cockpit Tools Codex quota cache.

**Architecture:** A small Go HTTP service reads Cockpit Tools files from a read-only mounted `/data` directory, sanitizes all account identifiers, renders an HTML dashboard, and exposes sanitized JSON endpoints for the page. The service never calls OpenAI and never writes to the mounted data.

**Tech Stack:** Go 1.23, `net/http`, `html/template`, `modernc.org/sqlite`, Docker, Woodpecker CI.

---

### Task 1: Read-Only Dashboard Service

**Files:**
- Create: `go.mod`
- Create: `main.go`
- Create: `main_test.go`

- [x] **Step 1: Implement account parsing and identity masking**

Read `/data/codex_accounts/*.json`, parse only the fields needed for display, and mask emails as `m***@**.com`.

- [x] **Step 2: Implement local API usage summary**

Read `codex_local_access_stats.json` when present. Fall back to `codex_local_access_logs.sqlite` with read-only SQLite mode.

- [x] **Step 3: Render an HTML dashboard**

Render account quotas, stale cache state, quota errors, and local API usage summaries with no raw identifiers.

- [x] **Step 4: Add tests**

Cover masking, sanitized account loading, and SQLite usage aggregation.

### Task 2: Container And CI

**Files:**
- Create: `Dockerfile`
- Create: `docker-compose.yml`
- Create: `.woodpecker/build.yaml`
- Create: `.gitignore`
- Create: `README.md`

- [x] **Step 1: Add Docker build**

Use a Go builder stage and a small Alpine runtime image with a non-root user.

- [x] **Step 2: Add Docker Compose**

Bind `127.0.0.1:8080:8080`, mount `${HOME}/.antigravity_cockpit:/data:ro`, set `read_only: true`, drop capabilities, and add a healthcheck.

- [x] **Step 3: Add Woodpecker pipeline**

Follow local project conventions: matrix `amd64`/`arm64`, push to `docker.nexus.ixuni.win/codex-quota-viewer`, use `nexus_username` and `nexus_password`, and notify failures via `woodpecker-mlntfy`.

- [x] **Step 4: Add README**

Document privacy behavior, data sources, endpoints, compose deployment, and build commands.
