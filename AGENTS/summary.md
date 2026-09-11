# Project State: MtG Prerelease Signup

## What it is
Single-binary Go web app for signing up to Magic: The Gathering prerelease events.
The organizer creates events on `/organizer`; each event is reached via its set
code at `/<set-code>`. Submissions are stored in SQLite and exportable as CSV
per event. Payment is manual via Wero, IBAN, or cash; payment is tracked in
database, users confirm payment.

## Stack
- Go 1.22 stdlib for HTTP, templates, CSV, SMTP (`net/smtp`), MIME (`mime.QEncoding` for RFC 2047 subject encoding), and embedding.
- `modernc.org/sqlite` for pure-Go SQLite (no CGO).
- `github.com/yeqown/go-qrcode/v2` for inline SVG QR code generation (EPC QR for SEPA, Wero QR).
- `html/template` for all pages with i18n support via `T()` FuncMap.
- Hosted on Fly.io with a persistent volume for `data.db`.
- Tests use the Go stdlib `testing` package only (no testify).

## Files
- `main.go` — all server code: DB setup, events model, handlers, SMTP mailer, cancellation/promotion logic, per-event CSV export, QR generation, payment page.
- `strings.go` — i18n catalog and translation functions (`T()`, `parseAcceptLanguage()`).
- `index.html` — server-side template for the per-event signup form with i18n, no JavaScript.
- `pay.html` — payment page with 3 options (Wero, IBAN, cash), inline SVG QRs, confirmation after payment recorded, i18n.
- `waitlist.html` — waitlist notice page with i18n.
- `cancel.html` — self-serve cancellation form with i18n.
- `organizer.html` — event list + creation form (English only, organizer-gated).
- `organizer_event.html` — per-event submissions table + edit form (English only, organizer-gated).
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
- `GET /` — redirects (302) to the latest event by `date DESC` (`SELECT set_code FROM events ORDER BY date DESC LIMIT 1`). If no events exist, 500.
- `GET /{set_code}` — redirects (302) to `/{set_code}/<lang>/` based on `Accept-Language`.
- `GET /{set_code}/{lang}/` — per-event signup form with live seat counts, lang = `en` or `de`. Unknown set code → 200 `msg_unknown_event`.
- `POST /{set_code}/{lang}/submit` — validates input (rejects CR/LF in email/name), resolves `event_id` from set code, inserts confirmed or waitlist row scoped to the event, sends localized email. Confirmed users redirect to `/{set_code}/{lang}/pay?email=...`, waitlist to `/{set_code}/{lang}/waitlist`.
- `GET /{set_code}/{lang}/pay?email=<urlencoded>` — payment page with amounts from the event (rendered as `€%.2f`), showing 3 collapsed `<details>` blocks (Wero/IBAN/cash); when `payment != unknown` also shows thanks banner (`recorded_payment`/`recorded_cash`) + `pay_later_note` above the collapsed blocks; static `pay_later_info` at bottom notes the confirmation mail contains the pay link. The Wero link carries the amount in whole cents (`?a=<cents>&c=EUR`); the EPC QR encodes a SEPA transfer with a two-decimal euro amount.
- `POST /{set_code}/{lang}/pay` — records payment method (wero/iban→`paid`, cash→`cash`), redirects back to GET.
- `GET /{set_code}/{lang}/waitlist` — waitlist notice, localized.
- `GET /{set_code}/{lang}/cancel` — cancellation form, localized.
- `POST /{set_code}/{lang}/cancel` — cancels by email (scoped to the event), promotes oldest waitlister for the same event + format, emails promoted person (in waiter's signup language) and notifies organizer (always English).
- `GET /organizer` — Session-cookie-protected page listing all events (newest first, date shown as `yyyy-mm-dd HH:MM`) with per-format signup counts plus a creation form. The create form is prefilled with sensible defaults (time `17:30`, bilingual What/Where text, Berlin address, per-language where links, draft price €12.50, sealed price €25). Unauthenticated requests redirect to `/organizer/login`.
- `POST /organizer` — validate + insert a new event (set code, date+time, what/where EN+DE, where link EN+DE, capacities, decimal prices). On validation failure re-renders the page with 400 + English error.
- `GET /organizer/{set_code}` — per-event submissions table + edit form (all event fields editable, prices as decimal `step="0.01"` inputs, where link split EN/DE). `?export=csv` exports that event's submissions (10 columns, last `set_code`). Unauthenticated → redirect to login.
- `POST /organizer/{set_code}` — update all event fields (set code editable with full validation). On failure re-renders with 400 + error.
- `GET /organizer/login` — login form (username pre-filled `organizer`, password field with `autocomplete="current-password"` for password-manager autofill).
- `POST /organizer/login` — verifies `username` == `organizer` and `password` == `ORGANIZER_PASSWORD` (constant-time compare); on success sets session cookie and redirects to `/organizer`, on failure re-renders form with error (401).
- `GET /health` — returns "ok".

## Internationalization (i18n)
- Supported languages: English (`en`) and German (`de`).
- Language selection: URL path segment after the set code (`/{set_code}/en/...`, `/{set_code}/de/...`).
- Root URL (`/`) redirects to the latest event; `/{set_code}` redirects by `Accept-Language`.
- Invalid lang segment redirects to `/{set_code}/en/...`.
- User-facing pages, forms, and emails are localized.
- Organizer pages (`organizer.html`, `organizer_event.html`, `organizer_login.html`) and organizer-facing validation errors are English-only hardcoded (no i18n keys).
- Emails are MIME-encoded for UTF-8: `Content-Type: text/plain; charset=utf-8`, `Content-Transfer-Encoding: 8bit`, and non-ASCII subjects are RFC 2047 encoded via `mime.QEncoding.Encode`.
- Waitlist/promotion emails use the waiter's signup language; organizer notifications are always English.
- Database stores `lang` column per submission for email language tracking.
- `strings.go` contains translation map and `T(lang, key, args...)` function used in templates. Email body strings (`body_confirmed`, `body_waitlist`) carry the set code in their URLs.

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

## Capacities
- Per-event: `draft_cap` and `sealed_cap` columns on `events` (set by the organizer at creation, editable).
- Over-cap signups become `waitlist`.

## Database Schema
- `events` table: `id`, `set_code` (UNIQUE), `date` (RFC3339), `what_en`, `what_de`, `where_en`, `where_de`, `where_link_en`, `where_link_de`, `draft_cap`, `sealed_cap`, `draft_price` (REAL), `sealed_price` (REAL). No `created_at` (id orders insertion, `date` orders display).
- `submissions` table: `id`, `email`, `name`, `format`, `mailing_list`, `created_at`, `payment`, `status`, `lang`, `event_id` (REFERENCES events). `UNIQUE(event_id, email)` — the same email may sign up to different events but only once per event.
- `payment` column states: `unknown`, `paid`, `cash`.
- `status` column states: `confirmed`, `waitlist`, `cancelled`.
- `lang` column: user's signup language (`en` or `de`), used for localized emails.
- **Schema bump**: existing `data.db` must be deleted on deploy (fresh start; no migration script).

## Set code validation
- Regex `^[A-Z0-9]{2,8}$` (2–8 uppercase alphanumerics).
- Reserved first-segments rejected at creation/edit: `organizer`, `health`, `events`, `new`, `en`, `de`.

## Payment Details
- Wero: QR code + manual "send to email" instructions; link amount in whole cents derived from the float price.
- IBAN: EPC QR (EPC069-12) encoding SEPA transfer, plus manual details (recipient, IBAN, BIC, amount, reference=`Prerelease <set_code> id <DB_ID>`).
- Cash: user marks intent, confirmed at event.
- Amounts come from the event row (`draft_price` / `sealed_price`, stored as REAL/float64 to support decimals like 12.50).

## Status
Implemented and tested with full i18n support (English + German) and the
multi-event model. Run `go test ./...` and `go vet ./...` to verify. `air`
handles builds — do not run `go build`/`make build` manually.
