package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"math"
	"mime"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/yeqown/go-qrcode/v2"
	_ "modernc.org/sqlite"
)

//go:embed index.html pay.html waitlist.html cancel.html organizer.html organizer_event.html organizer_login.html
var embeddedFiles embed.FS

type Mailer interface {
	Send(to, subject, body string) error
}

type smtpMailer struct {
	host, port, user, password, from string
}

func (m smtpMailer) Send(to, subject, body string) error {
	msg := buildMessage(m.from, to, subject, body)
	addr := m.host + ":" + m.port
	auth := smtp.PlainAuth("", m.user, m.password, m.host)
	return smtp.SendMail(addr, auth, m.from, []string{to}, msg)
}

const replyToAddress = "magicdraftberlin@posteo.de"

func buildMessage(from, to, subject, body string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Reply-To: " + replyToAddress + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n" +
		"\r\n" +
		body + "\r\n")
}

// config holds runtime configuration resolved once at startup so handlers
// never read os.Getenv directly and required values are validated up front.
type config struct {
	weroEmail         string
	weroLink          string
	iban              string
	ibanRecipient     string
	bic               string
	organizerEmail    string
	organizerPassword string
}

// loadConfig reads configuration from the environment and validates required
// values. Missing required values are fatal at startup so the server never
// boots into a state where payment/organizer emails would silently be empty.
func loadConfig() config {
	cfg := config{
		weroEmail:         os.Getenv("WERO_EMAIL"),
		weroLink:          os.Getenv("WERO_LINK"),
		iban:              os.Getenv("IBAN"),
		ibanRecipient:     os.Getenv("IBAN_RECIPIENT"),
		bic:               os.Getenv("BIC"),
		organizerEmail:    getenv("ORGANIZER_EMAIL", "diligence.dev@web.de"),
		organizerPassword: os.Getenv("ORGANIZER_PASSWORD"),
	}
	if cfg.weroEmail == "" {
		log.Fatalf("WERO_EMAIL not set")
	}
	if cfg.weroLink == "" {
		log.Fatalf("WERO_LINK not set")
	}
	if cfg.iban == "" {
		log.Fatalf("IBAN not set")
	}
	if cfg.ibanRecipient == "" {
		log.Fatalf("IBAN_RECIPIENT not set")
	}
	if cfg.bic == "" {
		log.Fatalf("BIC not set")
	}
	if cfg.organizerEmail == "" {
		log.Fatalf("ORGANIZER_EMAIL not set")
	}
	if cfg.organizerPassword == "" {
		log.Fatalf("ORGANIZER_PASSWORD not set")
	}
	return cfg
}

func main() {
	cfg := loadConfig()

	dataPath := getenv("DATA_PATH", "data.db")
	db, err := sql.Open("sqlite", dataPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		log.Fatalf("init schema: %v", err)
	}

	password := os.Getenv("SMTP_PASSWORD")
	if password == "" {
		log.Fatalf("SMTP_PASSWORD not set")
	}
	mailer := smtpMailer{
		host:     "smtp.web.de",
		port:     "587",
		user:     "diligence.bot@web.de",
		password: password,
		from:     getenv("SMTP_FROM", "diligence.bot@web.de"),
	}

	handler, err := setupHandlers(db, mailer, cfg)
	if err != nil {
		log.Fatalf("setup handlers: %v", err)
	}
	port := getenv("PORT", "8080")
	log.Printf("listening on :%s", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY,
		set_code TEXT UNIQUE NOT NULL,
		date TEXT NOT NULL,
		what_en TEXT NOT NULL,
		what_de TEXT NOT NULL,
		where_en TEXT NOT NULL,
		where_de TEXT NOT NULL,
		where_link_en TEXT NOT NULL DEFAULT '',
		where_link_de TEXT NOT NULL DEFAULT '',
		draft_cap INTEGER NOT NULL,
		sealed_cap INTEGER NOT NULL,
		draft_price REAL NOT NULL,
		sealed_price REAL NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS submissions (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL,
		name TEXT NOT NULL,
		format TEXT NOT NULL,
		mailing_list INTEGER DEFAULT 0,
		created_at TEXT NOT NULL,
		payment TEXT NOT NULL DEFAULT 'unknown',
		status TEXT NOT NULL DEFAULT 'confirmed',
		lang TEXT NOT NULL DEFAULT 'en',
		event_id INTEGER NOT NULL REFERENCES events(id),
		UNIQUE(event_id, email)
	)`)
	return err
}

func setupHandlers(db *sql.DB, mailer Mailer, cfg config) (http.Handler, error) {
	funcMap := template.FuncMap{"T": T}

	indexTmpl, err := template.New("index.html").Funcs(funcMap).ParseFS(embeddedFiles, "index.html")
	if err != nil {
		return nil, err
	}
	payTmpl, err := template.New("pay.html").Funcs(funcMap).ParseFS(embeddedFiles, "pay.html")
	if err != nil {
		return nil, err
	}
	waitlistTmpl, err := template.New("waitlist.html").Funcs(funcMap).ParseFS(embeddedFiles, "waitlist.html")
	if err != nil {
		return nil, err
	}
	cancelTmpl, err := template.New("cancel.html").Funcs(funcMap).ParseFS(embeddedFiles, "cancel.html")
	if err != nil {
		return nil, err
	}
	organizerTmpl, err := template.New("organizer.html").Funcs(funcMap).ParseFS(embeddedFiles, "organizer.html")
	if err != nil {
		return nil, err
	}
	organizerEventTmpl, err := template.New("organizer_event.html").Funcs(funcMap).ParseFS(embeddedFiles, "organizer_event.html")
	if err != nil {
		return nil, err
	}
	organizerLoginTmpl, err := template.New("organizer_login.html").Funcs(funcMap).ParseFS(embeddedFiles, "organizer_login.html")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", latestEventHandler(db))
	mux.HandleFunc("GET /{set_code}", eventLangRedirectHandler())
	mux.HandleFunc("GET /{set_code}/{$}", eventLangRedirectHandler())
	mux.HandleFunc("GET /{set_code}/{lang}/{$}", localizedHandler(indexHandler(db, indexTmpl)))
	mux.HandleFunc("POST /{set_code}/{lang}/submit", localizedHandler(submitHandler(db, mailer, cfg)))
	mux.HandleFunc("GET /{set_code}/{lang}/pay", localizedHandler(payHandler(db, payTmpl, cfg)))
	mux.HandleFunc("POST /{set_code}/{lang}/pay", localizedHandler(postPayHandler(db, cfg)))
	mux.HandleFunc("GET /{set_code}/{lang}/waitlist", localizedHandler(waitlistHandler(waitlistTmpl)))
	mux.HandleFunc("GET /{set_code}/{lang}/cancel", localizedHandler(cancelFormHandler(cancelTmpl)))
	mux.HandleFunc("POST /{set_code}/{lang}/cancel", localizedHandler(cancelHandler(db, mailer, cfg)))
	mux.HandleFunc("GET /organizer", organizerHandler(db, organizerTmpl, cfg))
	mux.HandleFunc("POST /organizer", organizerCreatePostHandler(db, organizerTmpl, cfg))
	mux.HandleFunc("GET /organizer/{set_code}", organizerEventHandler(db, organizerEventTmpl, cfg))
	mux.HandleFunc("POST /organizer/{set_code}", organizerEventPostHandler(db, organizerEventTmpl, cfg))
	mux.HandleFunc("GET /organizer/login", organizerLoginHandler(organizerLoginTmpl))
	mux.HandleFunc("POST /organizer/login", organizerLoginPostHandler(cfg, organizerLoginTmpl))
	mux.HandleFunc("GET /health", healthHandler)
	return mux, nil
}

// latestEventHandler redirects "/" to the most recent event by date. If no
// event exists the query returns no rows and the error surfaces as 500 (no
// special handling, per the multi-event plan decision 6).
func latestEventHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var setCode string
		err := db.QueryRow("SELECT set_code FROM events ORDER BY date DESC LIMIT 1").Scan(&setCode)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "no events")
			return
		}
		http.Redirect(w, r, "/"+setCode, http.StatusFound)
	}
}

// eventLangRedirectHandler redirects "/{set_code}" to the user's preferred
// language based on the Accept-Language header.
func eventLangRedirectHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setCode := r.PathValue("set_code")
		lang := parseAcceptLanguage(r.Header.Get("Accept-Language"))
		http.Redirect(w, r, "/"+setCode+"/"+lang+"/", http.StatusFound)
	}
}

func localizedHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		if lang != "en" && lang != "de" {
			setCode := r.PathValue("set_code")
			rest := strings.TrimPrefix(r.URL.Path, "/"+setCode+"/"+lang)
			target := "/" + setCode + "/en" + rest
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		h(w, r)
	}
}

// event holds one row of the events table.
type event struct {
	ID          int64
	SetCode     string
	Date        string
	WhatEn      string
	WhatDe      string
	WhereEn     string
	WhereDe     string
	WhereLinkEn string
	WhereLinkDe string
	DraftCap    int
	SealedCap   int
	DraftPrice  float64
	SealedPrice float64
}

func loadEventBySetCode(db *sql.DB, setCode string) (event, error) {
	var e event
	err := db.QueryRow(`SELECT id, set_code, date, what_en, what_de, where_en, where_de, where_link_en, where_link_de, draft_cap, sealed_cap, draft_price, sealed_price FROM events WHERE set_code=?`, setCode).
		Scan(&e.ID, &e.SetCode, &e.Date, &e.WhatEn, &e.WhatDe, &e.WhereEn, &e.WhereDe, &e.WhereLinkEn, &e.WhereLinkDe, &e.DraftCap, &e.SealedCap, &e.DraftPrice, &e.SealedPrice)
	return e, err
}

var setCodeRe = regexp.MustCompile(`^[A-Z0-9]{2,8}$`)

var reservedSetCodes = map[string]bool{
	"organizer": true,
	"health":    true,
	"events":    true,
	"new":       true,
	"en":        true,
	"de":        true,
}

// validSetCode reports whether code matches the format regex and is not a
// reserved first-segment that would collide with an existing route.
func validSetCode(code string) bool {
	return setCodeRe.MatchString(code) && !reservedSetCodes[code]
}

var deMonths = map[string]string{
	"January": "Januar", "February": "Februar", "March": "März",
	"April": "April", "May": "Mai", "June": "Juni",
	"July": "Juli", "August": "August", "September": "September",
	"October": "Oktober", "November": "November", "December": "Dezember",
}

// formatDate renders an RFC3339 date in a human format, localised per lang.
func formatDate(dateStr, lang string) string {
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		return dateStr
	}
	month := t.Format("January")
	if lang == "de" {
		if de, ok := deMonths[month]; ok {
			month = de
		}
		return fmt.Sprintf("%d. %s %d um %02d:%02d Uhr", t.Day(), month, t.Year(), t.Hour(), t.Minute())
	}
	return fmt.Sprintf("%d %s %d at %02d:%02d", t.Day(), month, t.Year(), t.Hour(), t.Minute())
}

type indexPageData struct {
	Lang            string
	SetCode         string
	EventDate       string
	WhatText        string
	WhereText       string
	WhereLink       string
	DraftPrice      float64
	SealedPrice     float64
	DraftSeatsLeft  int
	SealedSeatsLeft int
}

func indexHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")
		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusOK, T(lang, "msg_unknown_event"))
			return
		}
		draftLeft, err := seatsLeft(db, e.ID, "draft", e.DraftCap)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}
		sealedLeft, err := seatsLeft(db, e.ID, "sealed", e.SealedCap)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}
		whatText, whereText := e.WhatEn, e.WhereEn
		whereLink := e.WhereLinkEn
		if lang == "de" {
			whatText, whereText = e.WhatDe, e.WhereDe
			whereLink = e.WhereLinkDe
		}
		data := indexPageData{
			Lang:            lang,
			SetCode:         setCode,
			EventDate:       formatDate(e.Date, lang),
			WhatText:        whatText,
			WhereText:       whereText,
			WhereLink:       whereLink,
			DraftPrice:      e.DraftPrice,
			SealedPrice:     e.SealedPrice,
			DraftSeatsLeft:  draftLeft,
			SealedSeatsLeft: sealedLeft,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("index template execute: %v", err)
		}
	}
}

func seatsLeft(db *sql.DB, eventID int64, format string, cap int) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND event_id=? AND status='confirmed'", format, eventID).Scan(&count)
	if err != nil {
		return 0, err
	}
	left := cap - count
	if left < 0 {
		left = 0
	}
	return left, nil
}

type waitlistData struct {
	Lang    string
	SetCode string
}

func waitlistHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, waitlistData{Lang: lang, SetCode: setCode}); err != nil {
			log.Printf("waitlist template execute: %v", err)
		}
	}
}

type cancelData struct {
	Lang    string
	SetCode string
}

func cancelFormHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, cancelData{Lang: lang, SetCode: setCode}); err != nil {
			log.Printf("cancel template execute: %v", err)
		}
	}
}

// writeText writes a plain-text response with the given status code, replacing
// the http.Error-based pattern so 2xx responses are no longer sent via an
// error helper.
func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprint(w, msg)
}

// hasNewline reports whether s contains a carriage return or newline. Used to
// reject form fields that could otherwise inject SMTP/HTTP headers.
func hasNewline(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

func submitHandler(db *sql.DB, mailer Mailer, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")

		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusOK, T(lang, "msg_unknown_event"))
			return
		}

		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_form"))
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		name := strings.TrimSpace(r.FormValue("name"))
		format := r.FormValue("format")
		cancellationAck := r.FormValue("cancellation_ack")
		dataConsent := r.FormValue("data_consent")
		mailingList := r.FormValue("mailing_list")

		switch {
		case email == "" || !strings.Contains(email, "@") || hasNewline(email):
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_email"))
			return
		case name == "" || len(name) > 200 || hasNewline(name):
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_name"))
			return
		case format != "draft" && format != "sealed":
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_format"))
			return
		case cancellationAck != "on":
			writeText(w, http.StatusBadRequest, T(lang, "msg_cancel_ack_required"))
			return
		case dataConsent != "on":
			writeText(w, http.StatusBadRequest, T(lang, "msg_data_consent_required"))
			return
		}

		cap := e.DraftCap
		amount := e.DraftPrice
		if format == "sealed" {
			cap = e.SealedCap
			amount = e.SealedPrice
		}

		mailingListInt := 0
		if mailingList == "yes" {
			mailingListInt = 1
		}

		tx, err := db.Begin()
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}
		defer tx.Rollback()

		var confirmedCount int
		err = tx.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND event_id=? AND status='confirmed'", format, e.ID).Scan(&confirmedCount)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		status := "confirmed"
		if confirmedCount >= cap {
			status = "waitlist"
		}

		createdAt := time.Now().UTC().Format(time.RFC3339)
		_, err = tx.Exec(
			"INSERT INTO submissions (email, name, format, mailing_list, created_at, status, lang, event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			email, name, format, mailingListInt, createdAt, status, lang, e.ID,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				writeText(w, http.StatusConflict, T(lang, "msg_already_registered"))
				return
			}
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		if err := tx.Commit(); err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		host := "https://" + r.Host

		var subject, body string
		if status == "confirmed" {
			subject = T(lang, "subject_confirmed")
			body = confirmationEmailBody(name, format, email, host, setCode, lang)
		} else {
			subject = T(lang, "subject_waitlist")
			body = T(lang, "body_waitlist", name, formatName(format), seatTotal(format, cap), host, setCode, lang)
		}

		if err := mailer.Send(email, subject, body); err != nil {
			log.Printf("failed to send %s email to %s: %v", status, email, err)
			amountLabel := formatEuro(amount)
			if status == "waitlist" {
				amountLabel = "waitlist"
			}
			failSubject := fmt.Sprintf("Failed to send payment mail to %s", email)
			failBody := fmt.Sprintf("Timestamp: %s\nName: %s\nEmail: %s\nFormat: %s\nAmount: %s\nError: %v",
				time.Now().UTC().Format(time.RFC3339), name, email, format, amountLabel, err)
			if notifyErr := mailer.Send(cfg.organizerEmail, failSubject, failBody); notifyErr != nil {
				log.Printf("failed to send failure notification: %v", notifyErr)
			}
		}

		if status == "confirmed" {
			http.Redirect(w, r, "/"+setCode+"/"+lang+"/pay?email="+url.QueryEscape(email), http.StatusFound)
		} else {
			http.Redirect(w, r, "/"+setCode+"/"+lang+"/waitlist", http.StatusFound)
		}
	}
}

func formatName(format string) string {
	if format == "draft" {
		return "Draft"
	}
	return "Sealed"
}

func seatTotal(format string, cap int) string {
	return fmt.Sprintf("%d %s", cap, formatName(format))
}

func confirmationEmailBody(name, format, email, host, setCode, lang string) string {
	return T(lang, "body_confirmed", name, formatName(format), host, setCode, lang, url.QueryEscape(email), host, setCode, lang)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

type organizerRow struct {
	ID      int
	Email   string
	Name    string
	Format  string
	Payment string
	Status  string
	Italic  bool
	Strike  bool
}

// organizerEventListItem is one row of the event list on /organizer.
type organizerEventListItem struct {
	SetCode     string
	Date        string
	DraftCount  int
	DraftCap    int
	SealedCount int
	SealedCap   int
}

// organizerPageData backs organizer.html: the create form plus the event list.
type organizerPageData struct {
	Events []organizerEventListItem
	Error  string
}

// organizerEventPageData backs organizer_event.html: the edit form plus the
// per-event submission table.
type organizerEventPageData struct {
	Event       event
	DateInput   string
	TimeInput   string
	Rows        []organizerRow
	DraftCount  int
	SealedCount int
	Error       string
}

// signupCount returns the number of confirmed plus waitlist submissions for
// the given event and format (cancelled rows excluded).
func signupCount(db *sql.DB, eventID int64, format string) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND event_id=? AND status IN ('confirmed','waitlist')", format, eventID).Scan(&count)
	return count, err
}

// loadOrganizerEventList returns all events with per-format signup counts,
// newest event first.
func loadOrganizerEventList(db *sql.DB) ([]organizerEventListItem, error) {
	rows, err := db.Query(`SELECT set_code, date, draft_cap, sealed_cap,
		(SELECT COUNT(*) FROM submissions WHERE event_id=e.id AND format='draft' AND status IN ('confirmed','waitlist')),
		(SELECT COUNT(*) FROM submissions WHERE event_id=e.id AND format='sealed' AND status IN ('confirmed','waitlist'))
		FROM events e ORDER BY date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []organizerEventListItem
	for rows.Next() {
		var it organizerEventListItem
		var dateStr string
		if err := rows.Scan(&it.SetCode, &dateStr, &it.DraftCap, &it.SealedCap, &it.DraftCount, &it.SealedCount); err != nil {
			return nil, err
		}
		it.Date = dateStr
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			it.Date = t.Format("2006-01-02 15:04")
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

const sessionCookieName = "organizer_session"
const sessionMaxAge = 30 * 24 * time.Hour

// sessionKey derives a fixed-length HMAC key from the organizer password so
// the signing key has full entropy regardless of password length.
func sessionKey(cfg config) []byte {
	sum := sha256.Sum256([]byte(cfg.organizerPassword))
	return sum[:]
}

// makeSessionCookie builds a signed cookie authenticating the organizer until
// expiry. The cookie value is "<expiryUnix>.<base64url(hmac-sha256(key, expiryUnix))>".
func makeSessionCookie(cfg config, expiry time.Time) *http.Cookie {
	expiryStr := strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, sessionKey(cfg))
	mac.Write([]byte(expiryStr))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	value := expiryStr + "." + sig
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/organizer",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}

// validSession reports whether the request carries a valid, unexpired
// session cookie signed with the organizer password.
func validSession(r *http.Request, cfg config) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	expiryStr, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, sessionKey(cfg))
	mac.Write([]byte(expiryStr))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return false
	}
	return time.Unix(expiry, 0).After(time.Now())
}

type loginPageData struct {
	Error string
}

func organizerLoginHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.Execute(w, loginPageData{}); err != nil {
			log.Printf("organizer login template execute: %v", err)
		}
	}
}

func organizerLoginPostHandler(cfg config, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, "bad form")
			return
		}
		user := r.FormValue("username")
		pass := r.FormValue("password")
		if user != "organizer" || subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.organizerPassword)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			if err := tmpl.Execute(w, loginPageData{Error: "invalid credentials"}); err != nil {
				log.Printf("organizer login template execute: %v", err)
			}
			return
		}
		http.SetCookie(w, makeSessionCookie(cfg, time.Now().Add(sessionMaxAge)))
		http.Redirect(w, r, "/organizer", http.StatusFound)
	}
}

// renderOrganizerPage renders the organizer list/create page. When errMsg is
// non-empty it responds 400 so validation failures are signalled distinctly.
func renderOrganizerPage(w http.ResponseWriter, tmpl *template.Template, items []organizerEventListItem, errMsg string) {
	if errMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, organizerPageData{Events: items, Error: errMsg}); err != nil {
		log.Printf("organizer template execute: %v", err)
	}
}

func organizerHandler(db *sql.DB, tmpl *template.Template, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r, cfg) {
			http.Redirect(w, r, "/organizer/login", http.StatusFound)
			return
		}
		items, err := loadOrganizerEventList(db)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		renderOrganizerPage(w, tmpl, items, "")
	}
}

// parsePositiveInt parses a strictly positive integer form field.
func parsePositiveInt(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parsePositiveFloat parses a strictly positive decimal form field.
func parsePositiveFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return f, true
}

// formatEuro renders a price as a two-decimal euro string.
func formatEuro(amount float64) string {
	return fmt.Sprintf("€%.2f", amount)
}

// combineDateTime joins a "2006-01-02" date and "15:04" time into RFC3339 UTC.
func combineDateTime(date, timeStr string) (string, bool) {
	t, err := time.Parse("2006-01-02T15:04", date+"T"+timeStr)
	if err != nil {
		return "", false
	}
	return t.UTC().Format(time.RFC3339), true
}

func organizerCreatePostHandler(db *sql.DB, tmpl *template.Template, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r, cfg) {
			http.Redirect(w, r, "/organizer/login", http.StatusFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, "bad form")
			return
		}
		setCode := strings.TrimSpace(r.FormValue("set_code"))
		date := strings.TrimSpace(r.FormValue("date"))
		timeStr := strings.TrimSpace(r.FormValue("time"))
		whatEn := strings.TrimSpace(r.FormValue("what_en"))
		whatDe := strings.TrimSpace(r.FormValue("what_de"))
		whereEn := strings.TrimSpace(r.FormValue("where_en"))
		whereDe := strings.TrimSpace(r.FormValue("where_de"))
		whereLinkEn := strings.TrimSpace(r.FormValue("where_link_en"))
		whereLinkDe := strings.TrimSpace(r.FormValue("where_link_de"))
		draftCap, okD := parsePositiveInt(r.FormValue("draft_cap"))
		sealedCap, okS := parsePositiveInt(r.FormValue("sealed_cap"))
		draftPrice, okDP := parsePositiveFloat(r.FormValue("draft_price"))
		sealedPrice, okSP := parsePositiveFloat(r.FormValue("sealed_price"))

		items, _ := loadOrganizerEventList(db)
		rfc3339, okT := combineDateTime(date, timeStr)
		switch {
		case !validSetCode(setCode):
			renderOrganizerPage(w, tmpl, items, "invalid set code")
			return
		case !okT:
			renderOrganizerPage(w, tmpl, items, "invalid date or time")
			return
		case whatEn == "" || whatDe == "" || whereEn == "" || whereDe == "":
			renderOrganizerPage(w, tmpl, items, "what/where fields required")
			return
		case !okD || !okS || !okDP || !okSP:
			renderOrganizerPage(w, tmpl, items, "capacities must be positive integers and prices must be positive")
			return
		}

		_, err := db.Exec(`INSERT INTO events (set_code, date, what_en, what_de, where_en, where_de, where_link_en, where_link_de, draft_cap, sealed_cap, draft_price, sealed_price) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			setCode, rfc3339, whatEn, whatDe, whereEn, whereDe, whereLinkEn, whereLinkDe, draftCap, sealedCap, draftPrice, sealedPrice)
		if err != nil {
			renderOrganizerPage(w, tmpl, items, "set code already exists")
			return
		}
		http.Redirect(w, r, "/organizer/"+setCode, http.StatusFound)
	}
}

func organizerEventHandler(db *sql.DB, tmpl *template.Template, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r, cfg) {
			http.Redirect(w, r, "/organizer/login", http.StatusFound)
			return
		}
		setCode := r.PathValue("set_code")
		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusNotFound, "event not found")
			return
		}

		rows, err := db.Query(`SELECT id, email, name, format, mailing_list, created_at, payment, status, lang
			FROM submissions WHERE event_id=? ORDER BY CASE status
				WHEN 'confirmed' THEN 0
				WHEN 'waitlist'  THEN 1
				WHEN 'cancelled' THEN 2
				ELSE 3
			END, name COLLATE NOCASE ASC`, e.ID)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		defer rows.Close()

		if r.URL.Query().Get("export") == "csv" {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.csv\"", setCode))
			writer := csv.NewWriter(w)
			if err := writer.Write([]string{"id", "email", "name", "format", "mailing_list", "created_at", "payment", "status", "lang", "set_code"}); err != nil {
				log.Printf("csv write header: %v", err)
				return
			}
			for rows.Next() {
				var id, mailingList int
				var email, name, format, createdAt, payment, status, lang string
				if err := rows.Scan(&id, &email, &name, &format, &mailingList, &createdAt, &payment, &status, &lang); err != nil {
					log.Printf("csv scan row: %v", err)
					return
				}
				if err := writer.Write([]string{
					fmt.Sprintf("%d", id),
					email, name, format,
					fmt.Sprintf("%d", mailingList),
					createdAt, payment, status, lang,
					setCode,
				}); err != nil {
					log.Printf("csv write row: %v", err)
					return
				}
			}
			writer.Flush()
			if err := writer.Error(); err != nil {
				log.Printf("csv flush: %v", err)
			}
			return
		}

		var organizerRows []organizerRow
		for rows.Next() {
			var id, mailingList int
			var email, name, format, createdAt, payment, status, lang string
			if err := rows.Scan(&id, &email, &name, &format, &mailingList, &createdAt, &payment, &status, &lang); err != nil {
				log.Printf("organizer scan row: %v", err)
				return
			}
			organizerRows = append(organizerRows, organizerRow{
				ID:      id,
				Email:   email,
				Name:    name,
				Format:  format,
				Payment: payment,
				Status:  status,
				Italic:  format == "sealed",
				Strike:  status == "waitlist" || status == "cancelled",
			})
		}

		draftCount, err := signupCount(db, e.ID, "draft")
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		sealedCount, err := signupCount(db, e.ID, "sealed")
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}

		dateInput, timeInput := "", ""
		if t, err := time.Parse(time.RFC3339, e.Date); err == nil {
			dateInput = t.Format("2006-01-02")
			timeInput = t.Format("15:04")
		}
		data := organizerEventPageData{
			Event:       e,
			DateInput:   dateInput,
			TimeInput:   timeInput,
			Rows:        organizerRows,
			DraftCount:  draftCount,
			SealedCount: sealedCount,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("organizer event template execute: %v", err)
		}
	}
}

func renderOrganizerEventPage(w http.ResponseWriter, tmpl *template.Template, data organizerEventPageData, errMsg string) {
	data.Error = errMsg
	if errMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("organizer event template execute: %v", err)
	}
}

func organizerEventPostHandler(db *sql.DB, tmpl *template.Template, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r, cfg) {
			http.Redirect(w, r, "/organizer/login", http.StatusFound)
			return
		}
		setCode := r.PathValue("set_code")
		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusNotFound, "event not found")
			return
		}
		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, "bad form")
			return
		}
		newSetCode := strings.TrimSpace(r.FormValue("set_code"))
		date := strings.TrimSpace(r.FormValue("date"))
		timeStr := strings.TrimSpace(r.FormValue("time"))
		whatEn := strings.TrimSpace(r.FormValue("what_en"))
		whatDe := strings.TrimSpace(r.FormValue("what_de"))
		whereEn := strings.TrimSpace(r.FormValue("where_en"))
		whereDe := strings.TrimSpace(r.FormValue("where_de"))
		whereLinkEn := strings.TrimSpace(r.FormValue("where_link_en"))
		whereLinkDe := strings.TrimSpace(r.FormValue("where_link_de"))
		draftCap, okD := parsePositiveInt(r.FormValue("draft_cap"))
		sealedCap, okS := parsePositiveInt(r.FormValue("sealed_cap"))
		draftPrice, okDP := parsePositiveFloat(r.FormValue("draft_price"))
		sealedPrice, okSP := parsePositiveFloat(r.FormValue("sealed_price"))

		// Pre-build the page data for the error re-render path.
		dateInput, timeInput := date, timeStr
		data := organizerEventPageData{Event: e, DateInput: dateInput, TimeInput: timeInput}

		rfc3339, okT := combineDateTime(date, timeStr)
		switch {
		case !validSetCode(newSetCode):
			renderOrganizerEventPage(w, tmpl, data, "invalid set code")
			return
		case !okT:
			renderOrganizerEventPage(w, tmpl, data, "invalid date or time")
			return
		case whatEn == "" || whatDe == "" || whereEn == "" || whereDe == "":
			renderOrganizerEventPage(w, tmpl, data, "what/where fields required")
			return
		case !okD || !okS || !okDP || !okSP:
			renderOrganizerEventPage(w, tmpl, data, "capacities must be positive integers and prices must be positive")
			return
		}

		_, err = db.Exec(`UPDATE events SET set_code=?, date=?, what_en=?, what_de=?, where_en=?, where_de=?, where_link_en=?, where_link_de=?, draft_cap=?, sealed_cap=?, draft_price=?, sealed_price=? WHERE id=?`,
			newSetCode, rfc3339, whatEn, whatDe, whereEn, whereDe, whereLinkEn, whereLinkDe, draftCap, sealedCap, draftPrice, sealedPrice, e.ID)
		if err != nil {
			renderOrganizerEventPage(w, tmpl, data, "set code already exists")
			return
		}
		http.Redirect(w, r, "/organizer/"+newSetCode, http.StatusFound)
	}
}

func cancelHandler(db *sql.DB, mailer Mailer, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")

		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusOK, T(lang, "msg_unknown_event"))
			return
		}

		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_form"))
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		if email == "" || !strings.Contains(email, "@") || hasNewline(email) {
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_email"))
			return
		}

		var id int
		var name, format, status string
		err = db.QueryRow("SELECT id, name, format, status FROM submissions WHERE event_id=? AND email=?", e.ID, email).Scan(&id, &name, &format, &status)
		if err == sql.ErrNoRows {
			writeText(w, http.StatusOK, T(lang, "msg_no_registration"))
			return
		}
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		if status == "cancelled" {
			writeText(w, http.StatusOK, T(lang, "msg_already_cancelled"))
			return
		}

		host := "https://" + r.Host

		if status == "confirmed" {
			tx, err := db.Begin()
			if err != nil {
				writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
				return
			}
			defer tx.Rollback()

			_, err = tx.Exec("UPDATE submissions SET status='cancelled' WHERE id=?", id)
			if err != nil {
				writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
				return
			}

			var promotedID int
			var promotedEmail, promotedName, promotedLang string
			err = tx.QueryRow("SELECT id, email, name, lang FROM submissions WHERE format=? AND event_id=? AND status='waitlist' ORDER BY created_at ASC LIMIT 1", format, e.ID).Scan(&promotedID, &promotedEmail, &promotedName, &promotedLang)
			promoted := err == nil
			if promoted {
				_, err = tx.Exec("UPDATE submissions SET status='confirmed' WHERE id=?", promotedID)
				if err != nil {
					writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
					return
				}
			}

			if err := tx.Commit(); err != nil {
				writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
				return
			}
			amount := e.DraftPrice
			if format == "sealed" {
				amount = e.SealedPrice
			}

			if promoted {
				subject := T(promotedLang, "subject_confirmed")
				body := confirmationEmailBody(promotedName, format, promotedEmail, host, setCode, promotedLang)
				if err := mailer.Send(promotedEmail, subject, body); err != nil {
					log.Printf("failed to send promotion email to %s: %v", promotedEmail, err)
					failSubject := fmt.Sprintf("Failed to send payment mail to %s", promotedEmail)
					failBody := fmt.Sprintf("Timestamp: %s\nName: %s\nEmail: %s\nFormat: %s\nAmount: %s\nError: %v",
						time.Now().UTC().Format(time.RFC3339), promotedName, promotedEmail, format, formatEuro(amount), err)
					if notifyErr := mailer.Send(cfg.organizerEmail, failSubject, failBody); notifyErr != nil {
						log.Printf("failed to send failure notification: %v", notifyErr)
					}
				}
			}

			notifySubject := fmt.Sprintf("Cancelled: %s", email)
			var notifyBody string
			if promoted {
				notifyBody = fmt.Sprintf("%s (%s) cancelled their %s spot for %s. The seat went to %s (%s).", name, email, format, setCode, promotedName, promotedEmail)
			} else {
				notifyBody = fmt.Sprintf("%s (%s) cancelled their %s spot for %s. No one is on the waitlist for %s.", name, email, format, setCode, format)
			}
			if err := mailer.Send(cfg.organizerEmail, notifySubject, notifyBody); err != nil {
				log.Printf("failed to send organizer notification: %v", err)
			}

			writeText(w, http.StatusOK, T(lang, "msg_registration_cancelled"))
			return
		}

		if status == "waitlist" {
			_, err := db.Exec("UPDATE submissions SET status='cancelled' WHERE id=?", id)
			if err != nil {
				writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
				return
			}

			subject := fmt.Sprintf("Cancelled waitlist: %s", email)
			body := fmt.Sprintf("%s (%s) cancelled their %s waitlist spot for %s.", name, email, format, setCode)
			if err := mailer.Send(cfg.organizerEmail, subject, body); err != nil {
				log.Printf("failed to send organizer notification: %v", err)
			}

			writeText(w, http.StatusOK, T(lang, "msg_waitlist_cancelled"))
			return
		}

		writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
	}
}

type payPageData struct {
	Lang          string
	SetCode       string
	Email         string
	Format        string
	Amount        float64
	WeroEmail     string
	WeroLink      string
	WeroQR        template.HTML
	IBAN          string
	IBANRecipient string
	BIC           string
	Reference     string
	EpcQR         template.HTML
	Payment       string
}

func payHandler(db *sql.DB, tmpl *template.Template, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")
		email := strings.TrimSpace(r.URL.Query().Get("email"))
		if email == "" || !strings.Contains(email, "@") || hasNewline(email) {
			writeText(w, http.StatusOK, T(lang, "msg_not_available"))
			return
		}

		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusOK, T(lang, "msg_unknown_event"))
			return
		}

		var id int
		var format, payment, status string
		err = db.QueryRow("SELECT id, format, payment, status FROM submissions WHERE event_id=? AND email=?", e.ID, email).Scan(&id, &format, &payment, &status)
		if err == sql.ErrNoRows {
			writeText(w, http.StatusOK, T(lang, "msg_not_available"))
			return
		}
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		if status != "confirmed" {
			writeText(w, http.StatusOK, T(lang, "msg_not_available"))
			return
		}

		amount := e.DraftPrice
		if format == "sealed" {
			amount = e.SealedPrice
		}
		weroLinkWithAmount := fmt.Sprintf("%s?a=%d&c=EUR", cfg.weroLink, int(math.Round(amount*100)))

		reference := fmt.Sprintf("Prerelease %s id %d", setCode, id)
		epcPayloadText := epcPayload(cfg.bic, cfg.ibanRecipient, cfg.iban, amount, reference)
		epcQR, err := qrSVG(epcPayloadText, 8)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}
		weroQR, err := qrSVG(weroLinkWithAmount, 8)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		data := payPageData{
			Lang:          lang,
			SetCode:       setCode,
			Email:         email,
			Format:        formatName(format),
			Amount:        amount,
			WeroEmail:     cfg.weroEmail,
			WeroLink:      weroLinkWithAmount,
			WeroQR:        weroQR,
			IBAN:          cfg.iban,
			IBANRecipient: cfg.ibanRecipient,
			BIC:           cfg.bic,
			Reference:     reference,
			EpcQR:         epcQR,
			Payment:       payment,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("pay template execute: %v", err)
		}
	}
}

func postPayHandler(db *sql.DB, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		setCode := r.PathValue("set_code")
		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_form"))
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		method := r.FormValue("method")

		if email == "" || !strings.Contains(email, "@") || hasNewline(email) {
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_email"))
			return
		}

		if method != "wero" && method != "iban" && method != "cash" {
			writeText(w, http.StatusBadRequest, T(lang, "msg_invalid_method"))
			return
		}

		e, err := loadEventBySetCode(db, setCode)
		if err != nil {
			writeText(w, http.StatusOK, T(lang, "msg_unknown_event"))
			return
		}

		var id int
		var payment, status string
		err = db.QueryRow("SELECT id, payment, status FROM submissions WHERE event_id=? AND email=?", e.ID, email).Scan(&id, &payment, &status)
		if err == sql.ErrNoRows {
			writeText(w, http.StatusOK, T(lang, "msg_not_available"))
			return
		}
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		if status != "confirmed" {
			writeText(w, http.StatusOK, T(lang, "msg_not_available"))
			return
		}

		if payment != "unknown" {
			http.Redirect(w, r, "/"+setCode+"/"+lang+"/pay?email="+url.QueryEscape(email), http.StatusFound)
			return
		}

		newPayment := "paid"
		if method == "cash" {
			newPayment = "cash"
		}

		_, err = db.Exec("UPDATE submissions SET payment=? WHERE id=?", newPayment, id)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}

		http.Redirect(w, r, "/"+setCode+"/"+lang+"/pay?email="+url.QueryEscape(email), http.StatusFound)
	}
}

func epcPayload(bic, recipient, iban string, amount float64, reference string) string {
	amt := strings.Replace(fmt.Sprintf("%.2f", amount), ".", ",", 1)
	return fmt.Sprintf("BCD\r\n001\r\n1\r\nSCT\r\n%s\r\n%s\r\n%s\r\nEUR%s\r\n\r\n\r\n%s\r\n\r\n",
		bic, recipient, iban, amt, reference)
}

type svgWriter struct {
	b     strings.Builder
	scale int
}

func (w *svgWriter) Write(mat qrcode.Matrix) error {
	dim := mat.Width()
	size := dim * w.scale
	fmt.Fprintf(&w.b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, size, size, size, size)
	fmt.Fprintf(&w.b, `<rect width="%d" height="%d" fill="#fff"/>`, size, size)
	mat.Iterate(qrcode.IterDirection_COLUMN, func(x, y int, s qrcode.QRValue) {
		if s.IsSet() {
			fmt.Fprintf(&w.b, `<rect x="%d" y="%d" width="%d" height="%d" fill="#000"/>`, x*w.scale, y*w.scale, w.scale, w.scale)
		}
	})
	w.b.WriteString(`</svg>`)
	return nil
}

func (w *svgWriter) Close() error { return nil }

func qrSVG(text string, scale int) (template.HTML, error) {
	qrc, err := qrcode.New(text)
	if err != nil {
		return "", err
	}
	w := &svgWriter{scale: scale}
	if err := qrc.Save(w); err != nil {
		return "", err
	}
	return template.HTML(w.b.String()), nil
}
