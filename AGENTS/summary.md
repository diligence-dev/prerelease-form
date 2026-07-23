# Project State: MtG Prerelease Signup

## What it is
Single-binary Go web app for signing up to a Magic: The Gathering prerelease.
Submissions are stored in SQLite and exportable as CSV. Payment is manual via Wero; the organizer marks paid in the exported CSV after receiving Wero notifications.

## Stack
- Go 1.22 stdlib for HTTP, templates, CSV, SMTP (`net/smtp`), and embedding.
- `modernc.org/sqlite` for pure-Go SQLite (no CGO).
- `html/template` for the index form; `thanks.html` and `cancel.html` are static embedded files.
- Hosted on Fly.io with a persistent volume for `data.db`.
- Tests use the Go stdlib `testing` package only (no testify).

## Files
- `main.go` — all server code: DB setup, handlers, SMTP mailer, cancellation/promotion logic, CSV export.
- `index.html` — server-side template for the signup form, no JavaScript.
- `thanks.html` — static thanks page.
- `cancel.html` — static self-serve cancellation form.
- All three HTML pages share a centered card layout (inline radio options, styled inputs/buttons); styling is inline per page, no shared CSS file.
- `main_test.go` — table-driven tests with a fake `Mailer` and temp database.
- `go.mod` / `go.sum` — Go module files.
- `Dockerfile` — multi-stage scratch build.
- `fly.toml` — Fly.io configuration.
- `plan.md` — implementation plan.
- `AGENTS/AGENTS.md` — agent guidelines.
- `AGENTS/summary.md` — this file.

## Endpoints
- `GET /` — signup form with live seat counts.
- `POST /submit` — validates input (rejects CR/LF in email/name), inserts confirmed or waitlist row, sends email.
- `GET /thanks` — static thanks page.
- `GET /cancel` — static cancellation form.
- `POST /cancel` — cancels by email, promotes oldest waitlister for the same format, emails promoted person and notifies organizer (`ORGANIZER_EMAIL`).
- `GET /submissions.csv?token=<ADMIN_TOKEN>` — CSV export (constant-time token check).
- `GET /health` — returns "ok".

## Configuration
All configuration is resolved once at startup into a `config` struct via `loadConfig()`; handlers receive `cfg` and never read `os.Getenv` per-request. Missing required values are fatal at startup.

Required environment variables / Fly secrets:
- `ADMIN_TOKEN` — protects `/submissions.csv`.
- `SMTP_PASSWORD` — for `smtp.web.de` auth.
- `WERO_EMAIL` — shown in payment emails.
- `WERO_LINK` — payment link shown in payment emails.
- `IBAN` — shown in payment emails.
- `IBAN_RECIPIENT` — recipient name for IBAN payments.
- `BIC` — BIC code for IBAN payments.

Optional:
- `ORGANIZER_EMAIL` — recipient of cancellation/waitlist/delivery-failure notifications; defaults to `diligence.dev@web.de`.
- `SMTP_FROM` — defaults to `diligence.bot@web.de`.
- `PORT` — defaults to `8080`.
- `DATA_PATH` — defaults to `data.db`.

## Capacities
- Draft: 24 confirmed seats.
- Sealed: 8 confirmed seats.
- Over-cap signups become `waitlist`.

## Status
Implemented and tested. Run `go test ./...`, `go vet ./...`, and `CGO_ENABLED=0 go build` to verify.
