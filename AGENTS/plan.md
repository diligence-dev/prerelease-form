# Plan: MTG Prerelease Signup

## Goal
Single-binary web app for signing up to an MTG prerelease. Submissions stored in SQLite, exportable as CSV. No payment integration: participant pays via Wero manually; organizer marks `paid` in the exported CSV after receiving Wero notifications. On form submit the app emails the participant payment details (amount depends on format). The thanks page is static and JS-free; it only tells the user to expect an email.

## Stack
- **Go stdlib only** for HTTP, embedding, validation, CSV, and SMTP (`net/smtp`)
- `modernc.org/sqlite` — pure-Go SQLite driver (no CGO, static binary)
- `//go:embed` to bake `index.html`, `thanks.html`, and `cancel.html` into binary
- `html/template` to render `index.html` with live seat counts (stdlib); `thanks.html` and `cancel.html` are served as raw bytes
- `net/smtp` with PLAIN auth over STARTTLS to send payment, waitlist, promotion, and organizer-notification emails (stdlib, no new external dependency)
- Hosted on Fly.io with persistent volume for `data.db`
- Tests with `testing` stdlib (no testify)

## File layout (10 files)
```
main.go            # all server code
index.html         # form only, embedded, server-side template
thanks.html        # static thanks page, embedded
cancel.html        # static cancel form, embedded
main_test.go       # table-driven tests
go.mod
Dockerfile
fly.toml
plan.md            # this file
AGENTS/summary.md  # project state (required by AGENTS/AGENTS.md)
```

## Constants
```go
const draftCap, sealedCap = 24, 8
```

## Schema (data.db)
```sql
CREATE TABLE IF NOT EXISTS submissions (
  id INTEGER PRIMARY KEY,
  email TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  format TEXT NOT NULL,           -- 'draft' | 'sealed'
  mailing_list INTEGER NOT NULL,  -- 0 | 1
  created_at TEXT NOT NULL,        -- RFC3339 UTC
  paid INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'confirmed'  -- 'confirmed' | 'waitlist' | 'cancelled'
);
```
`amount` is derived server-side from `format` (draft=15, sealed=30) for the payment email; not stored. Cancellation identifies a row by `email`; no token column.

## index.html — server-side template, no JavaScript
- Parsed once at startup via `html/template`. `GET /` counts `status='confirmed'` rows per format and executes the template with `DraftSeatsLeft = draftCap - draftConfirmed` (clamped at 0) and `SealedSeatsLeft` similarly.
- Above the form, a short note: "Limited capacity: 24 Draft / 8 Sealed. Once full, further signups go on the waitlist — you'll get an email with payment details when a seat opens."
- Plain `<form method="POST" action="/submit">`, no `<script>`, no `<noscript>`, no thanks section.
- Fields per prompt:
  - Email (required, type=email)
  - Name (required)
  - Format radio: "Draft €15 — {{.DraftSeatsLeft}} left" / "Sealed €30 — {{.SealedSeatsLeft}} left" (required). At 0 left, the label reads e.g. "Sealed €30 — 0 left (waitlist)".
  - Cancellation ack checkbox (required): "I will email magicdraftberlin@posteo.de if I cannot come or am delayed"
  - Mailing list radio: yes / no (required)
  - Data processing consent checkbox (required)
  - Submit button
- Styling: minimal inline `<style>`, no external CSS. Mobile-first, readable. No frameworks.

## Thanks page (`/thanks`)
- Served from embedded `thanks.html` (200, Content-Type: text/html).
- Static content, no dynamic values, no JavaScript:
  > # Thanks!
  > You'll receive an email with payment details shortly. If you don't see it within a few minutes, check your spam folder.

## Cancel page (`/cancel`, `GET`)
- Served from embedded `cancel.html` (200, Content-Type: text/html). Static, no JavaScript.
- Plain `<form method="POST" action="/cancel">` with one email field (required, type=email) and a submit button. Brief text: "Enter the email address you signed up with to cancel your registration or waitlist spot."

## Environment variables (Fly secrets)
- `ADMIN_TOKEN` — required to access `/submissions.csv`
- `WERO_EMAIL` — used in payment email body (e.g. magicdraftberlin@posteo.de or your personal Wero email)
- `IBAN` — used in payment email body
- `SMTP_PASSWORD` — required for SMTP auth to `smtp.web.de`
- `SMTP_FROM` — optional, defaults to `diligence.bot@web.de`
- `SMTP_FAIL_NOTIFY` — optional, defaults to `diligence.dev@web.de`
- `PORT` — set by Fly (defaults to 8080)
- `DATA_PATH` — path to SQLite db (defaults to `data.db`)

Hardcoded SMTP settings (per spec): host `smtp.web.de`, port `587`, user `diligence.bot@web.de`, PLAIN auth over STARTTLS.

## Endpoints (main.go)

### `GET /`
Serve embedded `index.html` (200, Content-Type: text/html).

### `POST /submit`
1. Parse `application/x-www-form-urlencoded` body
2. Validate:
   - `email` non-empty and contains `@`
   - `name` non-empty, max 200 chars
   - `format` is exactly `draft` or `sealed`
   - `cancellation_ack` == `"on"` (mandatory checkbox)
   - `data_consent` == `"on"` (mandatory checkbox)
   - `mailing_list` is `"yes"` or `"no"`
3. On validation error: respond 400 with plain-text message listing first invalid field
4. On valid input, in a transaction:
   - `SELECT COUNT(*) FROM submissions WHERE format=? AND status='confirmed'`
   - If count < cap for that format: insert row with `status='confirmed'`; else insert row with `status='waitlist'`.
   - `created_at = time.Now().UTC().Format(time.RFC3339)`, `mailing_list` stored as 0/1.
5. On UNIQUE constraint violation (duplicate email): respond 409 with "You have already registered with this email."
6. On DB error other than UNIQUE: respond 500
7. After successful insert, build and send email based on the new row's `status`. Host for cancel links is built from the request's `Host` header as `https://<r.Host>` (works behind Fly's TLS-terminating proxy; no new env var).

   **confirmed** — payment email via `Mailer`:
   ```
   From: <SMTP_FROM>
   To: <submitter email>
   Subject: MTG Prerelease – Payment details

   Hi <name>,

   Thanks for signing up for <Draft|Sealed>.
   Please send €<15|30> via Wero to <WERO_EMAIL> (IBAN: <IBAN>).
   Use "<name>" as reference.
   We'll mark you paid once we receive the Wero notification.

   If you can no longer attend, cancel at https://<host>/cancel using your email address.
   ```
   **waitlist** — waitlist email via `Mailer` (no `€`, no `WERO_EMAIL`, no `IBAN`):
   ```
   From: <SMTP_FROM>
   To: <submitter email>
   Subject: MTG Prerelease – You're on the waitlist

   Hi <name>,

   Thanks for signing up for <Draft|Sealed>.
   All <24 draft / 8 sealed> seats are currently taken, so you've been added to the waitlist.
   You'll receive another email with payment details as soon as a seat opens up for you.

   If you no longer wish to be on the waitlist, cancel at https://<host>/cancel using your email address.
   ```
   - On send success: redirect (302) to `/thanks`.
   - On send failure: log the error, then send a failure-notification email to `<SMTP_FAIL_NOTIFY>` (default `diligence.dev@web.de`) with subject `Failed to send payment mail to <submitter email>` and body containing timestamp, submitter name, submitter email, format, amount (or "waitlist" when over cap), and the SMTP error message. Regardless of whether the failure-notification send succeeds or fails: redirect (302) to `/thanks`. The user always sees the thanks page.

### `GET /thanks`
Serve embedded `thanks.html` (200, Content-Type: text/html). Static, no dynamic content.

### `GET /cancel`
Serve embedded `cancel.html` (200, Content-Type: text/html). Static, no dynamic content.

### `POST /cancel`
1. Parse `application/x-www-form-urlencoded` body; read `email`.
2. Validate `email` non-empty and contains `@`; else respond 400 with plain text "Invalid email."
3. Look up row by `email`.
4. Not found → 200 with plain text "No registration found for that email." (Friendly response avoids leaking which addresses are registered.)
5. `status='cancelled'` → 200 with plain text "Your registration was already cancelled."
6. `status='confirmed'` — in a transaction:
   - `UPDATE submissions SET status='cancelled' WHERE id=?`
   - `SELECT id,email,name FROM submissions WHERE format=? AND status='waitlist' ORDER BY created_at ASC LIMIT 1`
   - If found: `UPDATE submissions SET status='confirmed' WHERE id=?` (promoted person). Commit.
   - If promoted: send the promotion (payment) email to the promoted person — identical body to the confirmed-signup payment email, with the promoted person's name/format/amount/Wero/IBAN and the cancel instructions. On failure → failure-notify to `<SMTP_FAIL_NOTIFY>`.
   - Send organizer notification to `<SMTP_FAIL_NOTIFY>` (best-effort, regardless of whether a promotion happened). Body:
     - promotion happened: `<name> (<email>) cancelled their <format> spot. The seat went to <promoted name> (<promoted email>).`
     - no waitlist for that format: `<name> (<email>) cancelled their <format> spot. No one is on the waitlist for <format>.`
   - Respond 200 with plain text "Your registration has been cancelled."
7. `status='waitlist'` — `UPDATE submissions SET status='cancelled'`, send organizer notification to `<SMTP_FAIL_NOTIFY>` (best-effort) with body `<name> (<email>) cancelled their <format> waitlist spot.`, respond 200 with plain text "Your waitlist spot has been cancelled."

### `GET /submissions.csv?token=<ADMIN_TOKEN>`
1. Compare `token` query param against `ADMIN_TOKEN` env var using `crypto/subtle.ConstantTimeCompare`
2. If missing or mismatch → 401 with plain "Unauthorized"
3. If match: query all rows ordered by `created_at DESC`
4. Write CSV response with columns: `id,email,name,format,mailing_list,created_at,paid,status`
5. Content-Type: `text/csv`, `Content-Disposition: attachment; filename="submissions.csv"`
6. `paid` exported as `0` or `1`; `status` exported as `confirmed`/`waitlist`/`cancelled` (organizer edits the CSV manually; no in-app mark-paid)

### `GET /health`
Returns 200 "ok" — for Fly health checks.

## main.go structure
```go
package main

import (
    "crypto/subtle"
    "database/sql"
    "embed"
    "encoding/csv"
    "fmt"
    "html/template"
    "net/http"
    "net/smtp"
    "os"
    "time"
    _ "modernc.org/sqlite"
)

//go:embed index.html thanks.html cancel.html
var indexHTML, thanksHTML, cancelHTML []byte

var db *sql.DB
var indexTmpl *template.Template

type Mailer interface {
    Send(to, subject, body string) error
}

type smtpMailer struct {
    host, port, user, password, from string
}

func (m smtpMailer) Send(to, subject, body string) error {
    // net/smtp: dial, StartTLS, PlainAuth, Mail, Rcpt, Data
}

func main() {
    // open db at env DATA_PATH or "data.db"
    // create schema if not exists
    // parse index.html as template
    // build smtpMailer from hardcoded host/user + env SMTP_PASSWORD, SMTP_FROM, SMTP_FAIL_NOTIFY
    // register handlers: /, /submit, /thanks, /cancel, /submissions.csv, /health
    // listen on env PORT or 8080
}

// handlers + helpers...
// seatsLeft(db, format) (int, error): cap minus count of status='confirmed' for that format, clamped at 0.
// cancelHandler: DB transaction for atomic cancel + promote oldest waitlister; send promotion and organizer emails.
```

`index.html` is executed via `html/template` with `{DraftSeatsLeft, SealedSeatsLeft int}`. `thanks.html` and `cancel.html` are served as raw bytes. No template execution for those two.

## Dockerfile
```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /app/server .

FROM scratch
COPY --from=build /app/server /server
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/server"]
```

## fly.toml
```toml
app = "mtg-prerelease"
primary_region = "fra"

[build]

[http_service]
  internal_port = 8080
  force_https = true

[mounts]
  source = "data"
  destination = "/data"

[env]
  DATA_PATH = "/data/data.db"
```

Deploy steps (documented in plan.md but executed by implementer):
- `flyctl apps create mtg-prerelease`
- `flyctl volumes create data --region fra`
- `flyctl secrets set ADMIN_TOKEN=... WERO_EMAIL=... IBAN=... SMTP_PASSWORD=...`
- `flyctl deploy`

## Test-first workflow (per AGENTS/AGENTS.md)

### main_test.go — write first, must FAIL before implementation
Table-driven tests using `httptest.NewServer` against the real `main()` handler wiring. Use a temp `data.db` via `t.TempDir()` for each test. A fake `Mailer` (implements `Mailer` interface) is injected via `setupHandlers(db, mailer) http.Handler` so tests record sends and simulate failures without hitting `smtp.web.de`.

Tests:
1. `GET /` returns 200 and body contains "Draft" and "Sealed", the initial seat numbers (`24`, `8`), and the waitlist note text
2. `POST /submit` with all valid fields → 302 redirect to `/thanks`
3. `POST /submit` missing email → 400
4. `POST /submit` missing name → 400
5. `POST /submit` missing format → 400
6. `POST /submit` unchecked cancellation_ack → 400
7. `POST /submit` unchecked data_consent → 400
8. `POST /submit` missing mailing_list → 400
9. `POST /submit` duplicate email → 409
10. `GET /submissions.csv` without token → 401
11. `GET /submissions.csv` with wrong token → 401
12. `GET /submissions.csv` with correct token → 200, Content-Type text/csv, body has header row (including `status`) + the submitted row (with `status=confirmed`)
13. `GET /health` → 200 "ok"
14. `GET /thanks` → 200, body contains "email with payment details"
15. Valid submit (draft) → fake Mailer recorded 1 send with `to` = submitter email, subject containing "Payment details", body containing "€15", the Wero email, the IBAN, and `/cancel`
16. Valid submit (sealed) → fake Mailer body contains "€30"
17. Valid submit, Mailer fails first Send → 2nd send to `diligence.dev@web.de` recorded, redirect still 302 → `/thanks`
18. Valid submit, Mailer fails both sends → still 302 → `/thanks`, no panic
19. Fill 24 confirmed draft rows → next draft `POST /submit` → 302 `/thanks`; DB row `status='waitlist'`; recorded email body contains "waitlist", does NOT contain "€15" or "IBAN"; contains `/cancel`
20. Same for sealed (8 confirmed → next is waitlist, body no `€30`/`IBAN`)
21. After 3 confirmed draft submissions, `GET /` body shows `21` left for draft and `8` for sealed
22. `GET /cancel` → 200, body contains an email input field and `action="/cancel"`
23. `POST /cancel` with email of a confirmed row that has one waitlist row for same format → 200; canceller `status='cancelled'`; oldest waitlist `status='confirmed'`; payment (promotion) email recorded to promoted person; organizer notification recorded to `SMTP_FAIL_NOTIFY` containing both names
24. `POST /cancel` with email of a confirmed row, no waitlist for that format → canceller cancelled; organizer notification body says `No one is on the waitlist`; no promotion email sent
25. `POST /cancel` with email of a waitlist row → `status='cancelled'`; no promotion email; organizer notification recorded
26. `POST /cancel` with email not in DB → 200 `No registration found`
27. `POST /cancel` with email of an already-cancelled row → 200 `already cancelled`

Tests must run against a refactored `main()` that exposes a `setupHandlers(db, mailer) http.Handler` function so tests wire a temp DB and fake Mailer without touching global state.

### Run tests (expected: fail)
```
go test ./...
```

### Implement main.go + index.html + thanks.html + cancel.html

### Run tests (expected: pass)
```
go test ./...
```

### Final checks
- `go vet ./...`
- `go build` succeeds with `CGO_ENABLED=0`
- No trailing whitespace in any file
- Newline at end of every file

## AGENTS.md compliance checklist
- [ ] Clarity: open questions resolved (above) before implementation
- [ ] Test first: write `main_test.go`, run, confirm all fail
- [ ] Implement, run tests, confirm all pass
- [ ] Clean code: no trailing whitespace, EOF newline on all files
- [ ] Update `AGENTS/summary.md` to reflect final project state
- [ ] Say "done" once all checks pass

## Explicit assumptions (for the implementing agent)
1. Wero manual flow: organizer gets P2P notification, then edits the CSV's `paid` column by hand. No automation.
2. No admin UI — CSV download is the only review surface.
3. `paid` is a column in the DB and CSV but never written by the app; organizer edits CSV in spreadsheet tool after download.
4. No rate limiting / CSRF protection — single event, small audience, low risk. Acceptable trade-off for simplicity.
5. Two signup-time emails: payment for `confirmed`, waitlist for over-cap. On cancellation, the app also sends a promotion (payment) email to the promoted waitlister and an organizer-notification email to `SMTP_FAIL_NOTIFY` (default `diligence.dev@web.de`). No other participant-facing emails.
6. No JavaScript — form is plain HTML, thanks and cancel pages are static.
7. `net/smtp` PLAIN auth over STARTTLS to `smtp.web.de:587` (stdlib, no new external dependency).
8. Email send is best-effort from the user's perspective: on failure the user still sees the thanks page; a failure-notification email goes to `diligence.dev@web.de` (or `SMTP_FAIL_NOTIFY`).
9. SMTP password lives in the `SMTP_PASSWORD` Fly secret, never in the repo.
10. Caps: 24 draft / 8 sealed; over-cap signups become `waitlist`. Live seat counts shown on the form via `html/template`.
11. Cancellation is self-serve via `GET /cancel` (form) + `POST /cancel` (email). Submitting the form atomically cancels the row and promotes the oldest `waitlist` row for the same format, emailing the promoted person payment details and notifying the organizer at `SMTP_FAIL_NOTIFY`. No per-row token: the row is identified by the submitted email.
12. Trade-off: anyone who knows a registrant's email can cancel their spot via `/cancel`. Accepted under assumption 4 (single event, small audience, low risk) in exchange for simplicity (no token column, no email round-trip to confirm intent).
