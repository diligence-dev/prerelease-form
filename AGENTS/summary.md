# Project State: MtG Prerelease Signup

## What it is
Single-binary Go web app for signing up to a Magic: The Gathering prerelease.
Submissions are stored in SQLite and exportable as CSV. Payment is manual via Wero, IBAN, or cash; payment is tracked in database, users confirm payment.

## Stack
- Go 1.22 stdlib for HTTP, templates, CSV, SMTP (`net/smtp`), and embedding.
- `modernc.org/sqlite` for pure-Go SQLite (no CGO).
- `github.com/yeqown/go-qrcode/v2` for inline SVG QR code generation (EPC QR for SEPA, Wero QR).
- `html/template` for the index and pay forms; `cancel.html` and `waitlist.html` are static embedded files.
- Hosted on Fly.io with a persistent volume for `data.db`.
- Tests use the Go stdlib `testing` package only (no testify).

## Files
- `main.go` — all server code: DB setup, handlers, SMTP mailer, cancellation/promotion logic, CSV export, QR generation, payment page.
- `index.html` — server-side template for the signup form, no JavaScript.
- `pay.html` — payment page with 3 options (Wero, IBAN, cash), inline SVG QRs, confirmation after payment recorded.
- `waitlist.html` — static waitlist notice page.
- `cancel.html` — static self-serve cancellation form.
- All HTML pages share a centered card layout (inline styles); no shared CSS file.
- `main_test.go` — table-driven tests with a fake `Mailer` and temp database.
- `go.mod` / `go.sum` — Go module files.
- `Dockerfile` — multi-stage scratch build.
- `fly.toml` — Fly.io configuration.
- `.github/workflows/fly.yml` — GitHub Action that runs `flyctl deploy --remote-only` on push to `main` (requires `FLY_API_TOKEN` repo secret).
- `plan.md` — implementation plan.
- `AGENTS/AGENTS.md` — agent guidelines.
- `AGENTS/summary.md` — this file.

## Endpoints
- `GET /` — signup form with live seat counts.
- `POST /submit` — validates input (rejects CR/LF in email/name), inserts confirmed or waitlist row, sends email. Confirmed users redirect to `/pay?email=...`, waitlist to `/waitlist`.
- `GET /pay?email=<urlencoded>` — payment page showing Wero/IBAN/cash options or confirmation if already paid.
- `POST /pay` — records payment method (wero/iban/cash), redirects back to GET.
- `GET /waitlist` — static waitlist notice.
- `GET /cancel` — static cancellation form.
- `POST /cancel` — cancels by email, promotes oldest waitlister for the same format, emails promoted person and notifies organizer.
- `GET /organizer` — Password-protected HTML table view of submissions (Basic Auth: username `organizer`, password `ORGANIZER_PASSWORD`). Shows columns: name, email, format, payment, id, status. Sorted by status (confirmed → waitlist → cancelled), then name alphabetically. Sealed rows italic, cancelled/waitlist rows strikethrough. A4 print-optimized.
- `GET /organizer?export=csv` — Same auth as HTML view, exports CSV with columns: id, email, name, format, mailing_list, created_at, payment, status.
- `GET /health` — returns "ok".

## Configuration
All configuration is resolved once at startup into a `config` struct via `loadConfig()`; handlers receive `cfg` and never read `os.Getenv` per-request. Missing required values are fatal at startup.

Required environment variables / Fly secrets:
- `ORGANIZER_PASSWORD` — protects `/submissions.csv`.
- `SMTP_PASSWORD` — for `smtp.web.de` auth.
- `WERO_EMAIL` — shown on payment page.
- `WERO_LINK` — payment link in Wero QR, also shown on payment page.
- `IBAN` — shown on payment page.
- `IBAN_RECIPIENT` — recipient name for IBAN payments.
- `BIC` — BIC code for IBAN payments.

Optional:
- `ORGANIZER_EMAIL` — recipient of cancellation/waitlist/delivery-failure notifications; defaults to `diligence.dev@web.de`.
- `SMTP_FROM` — defaults to `diligence.bot@web.de`.
- `PORT` — defaults to `8080`.
- `DATA_PATH` — defaults to `data.db`.
- `CAPACITY_DRAFT` — number of confirmed draft seats; defaults to 24.
- `CAPACITY_SEALED` — number of confirmed sealed seats; defaults to 8.

## Capacities
- Draft: configurable via `CAPACITY_DRAFT` (default 24) confirmed seats.
- Sealed: configurable via `CAPACITY_SEALED` (default 8) confirmed seats.
- Invalid capacity values (non-integer or <= 0) are fatal at startup.
- Over-cap signups become `waitlist`.

## Database Schema
- `submissions` table: `id`, `email`, `name`, `format`, `mailing_list`, `created_at`, `payment`, `status`
- `payment` column: `TEXT NOT NULL DEFAULT 'unknown'` — states are `unknown`, `paid`, `cash`
- `status` column: `confirmed`, `waitlist`, `cancelled`

## Payment Details
- Wero: QR code + manual "send to email" instructions
- IBAN: EPC QR (EPC069-12) encoding SEPA transfer, plus manual details (recipient, IBAN, BIC, amount, reference=db id)
- Cash: user marks intent, confirmed at event

## Status
Implemented and tested. Run `go test ./...` and `go vet ./...` to verify. `air` handles builds — do not run `go build`/`make build` manually.
