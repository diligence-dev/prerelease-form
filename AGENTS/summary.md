# Project State: MtG Prerelease Signup

## What it is
Single-binary Go web app for signing up to a Magic: The Gathering prerelease.
Submissions are stored in SQLite and exportable as CSV. Payment is manual via Wero, IBAN, or cash; payment is tracked in database, users confirm payment.

## Stack
- Go 1.22 stdlib for HTTP, templates, CSV, SMTP (`net/smtp`), MIME (`mime.QEncoding` for RFC 2047 subject encoding), and embedding.
- `modernc.org/sqlite` for pure-Go SQLite (no CGO).
- `github.com/yeqown/go-qrcode/v2` for inline SVG QR code generation (EPC QR for SEPA, Wero QR).
- `html/template` for all pages with i18n support via `T()` FuncMap.
- Hosted on Fly.io with a persistent volume for `data.db`.
- Tests use the Go stdlib `testing` package only (no testify).

## Files
- `main.go` — all server code: DB setup, handlers, SMTP mailer, cancellation/promotion logic, CSV export, QR generation, payment page.
- `strings.go` — i18n catalog and translation functions (`T()`, `parseAcceptLanguage()`).
- `index.html` — server-side template for the signup form with i18n, no JavaScript.
- `pay.html` — payment page with 3 options (Wero, IBAN, cash), inline SVG QRs, confirmation after payment recorded, i18n.
- `waitlist.html` — waitlist notice page with i18n.
- `cancel.html` — self-serve cancellation form with i18n.
- `organizer.html` — server-side template for the password-protected submissions table view (English only, A4 print-optimized).
- `organizer_login.html` — login form for `/organizer` (username + password, English only). Browser autofill-friendly so password managers save and fill credentials; on success sets a persistent 30-day session cookie.
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
- `GET /` — redirects to `/en/` or `/de/` based on `Accept-Language` header.
- `GET /{lang}/` — signup form with live seat counts, lang = `en` or `de`.
- `POST /{lang}/submit` — validates input (rejects CR/LF in email/name), inserts confirmed or waitlist row with lang, sends localized email. Confirmed users redirect to `/{lang}/pay?email=...`, waitlist to `/{lang}/waitlist`.
- `GET /{lang}/pay?email=<urlencoded>` — payment page showing Wero/IBAN/cash options or confirmation if already paid, localized.
- `POST /{lang}/pay` — records payment method (wero/iban/cash), redirects back to GET.
- `GET /{lang}/waitlist` — waitlist notice, localized.
- `GET /{lang}/cancel` — cancellation form, localized.
- `POST /{lang}/cancel` — cancels by email, promotes oldest waitlister for the same format, emails promoted person (in waiter's signup language) and notifies organizer (always English).
- `GET /organizer` — Session-cookie-protected HTML table view of submissions. Unauthenticated requests redirect to `/organizer/login`. Cookie is HMAC-SHA256 signed with a key derived from `ORGANIZER_PASSWORD` (via SHA-256), valid for 30 days (`HttpOnly; Secure; SameSite=Strict; Path=/organizer`). Shows columns: name, email, format, payment, id, status. Sorted by status (confirmed → waitlist → cancelled), then name alphabetically. Sealed rows italic, cancelled/waitlist rows strikethrough. A4 print-optimized. (English only)
- `GET /organizer/login` — login form (username pre-filled `organizer`, password field with `autocomplete="current-password"` for password-manager autofill).
- `POST /organizer/login` — verifies `username` == `organizer` and `password` == `ORGANIZER_PASSWORD` (constant-time compare); on success sets session cookie and redirects to `/organizer`, on failure re-renders form with error (401).
- `GET /organizer?export=csv` — Same auth as HTML view, exports CSV with ALL columns from database.
- `GET /health` — returns "ok".

## Internationalization (i18n)
- Supported languages: English (`en`) and German (`de`).
- Language selection: URL path prefix (`/en/...`, `/de/...`).
- Root URL (`/`) redirects based on `Accept-Language` header via stdlib-only parser.
- Invalid lang prefix redirects to `/en/...`.
- User-facing pages, forms, and emails are localized.
- Emails are MIME-encoded for UTF-8: `Content-Type: text/plain; charset=utf-8`, `Content-Transfer-Encoding: 8bit`, and non-ASCII subjects are RFC 2047 encoded via `mime.QEncoding.Encode` (so German umlauts and the en-dash in subjects render correctly in all mail clients).
- Waitlist/promotion emails use the waiter's signup language.
- Organizer-facing emails (failure notifications, cancel notices) are always in English.
- Database stores `lang` column per submission for email language tracking.
- `strings.go` contains translation map and `T(lang, key, args...)` function used in templates.

## Configuration
All configuration is resolved once at startup into a `config` struct via `loadConfig()`; handlers receive `cfg` and never read `os.Getenv` per-request. Missing required values are fatal at startup.

Required environment variables / Fly secrets:
- `ORGANIZER_PASSWORD` — required to log in at `/organizer/login`; also derives the HMAC signing key for the 30-day session cookie (via SHA-256). Rotating it invalidates all existing session cookies.
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
- `submissions` table: `id`, `email`, `name`, `format`, `mailing_list`, `created_at`, `payment`, `status`, `lang`
- `payment` column: `TEXT NOT NULL DEFAULT 'unknown'` — states are `unknown`, `paid`, `cash`
- `status` column: `confirmed`, `waitlist`, `cancelled`
- `lang` column: `TEXT NOT NULL DEFAULT 'en'` — user's signup language (`en` or `de`), used for localized emails.

## Payment Details
- Wero: QR code + manual "send to email" instructions
- IBAN: EPC QR (EPC069-12) encoding SEPA transfer, plus manual details (recipient, IBAN, BIC, amount, reference="Prerelease id $DB_ID")
- Cash: user marks intent, confirmed at event

## Status
Implemented and tested with full i18n support (English + German). Run `go test ./...` and `go vet ./...` to verify. `air` handles builds — do not run `go build`/`make build` manually.
