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
	"mime"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/yeqown/go-qrcode/v2"
	_ "modernc.org/sqlite"
)

//go:embed index.html pay.html waitlist.html cancel.html organizer.html organizer_login.html
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

func buildMessage(from, to, subject, body string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
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
	draftCap          int
	sealedCap         int
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
		draftCap:          getenvInt("CAPACITY_DRAFT", 24),
		sealedCap:         getenvInt("CAPACITY_SEALED", 8),
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

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Fatalf("%s invalid: %q", key, v)
	}
	return n
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS submissions (
		id INTEGER PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		format TEXT NOT NULL,
		mailing_list INTEGER DEFAULT 0,
		created_at TEXT NOT NULL,
		payment TEXT NOT NULL DEFAULT 'unknown',
		status TEXT NOT NULL DEFAULT 'confirmed',
		lang TEXT NOT NULL DEFAULT 'en'
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE submissions ADD COLUMN lang TEXT NOT NULL DEFAULT 'en'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
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
	organizerLoginTmpl, err := template.New("organizer_login.html").Funcs(funcMap).ParseFS(embeddedFiles, "organizer_login.html")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", rootRedirectHandler())
	mux.HandleFunc("GET /{lang}/{$}", localizedHandler(indexHandler(db, indexTmpl, cfg.draftCap, cfg.sealedCap)))
	mux.HandleFunc("POST /{lang}/submit", localizedHandler(submitHandler(db, mailer, cfg)))
	mux.HandleFunc("GET /{lang}/pay", localizedHandler(payHandler(db, payTmpl, cfg)))
	mux.HandleFunc("POST /{lang}/pay", localizedHandler(postPayHandler(db, cfg)))
	mux.HandleFunc("GET /{lang}/waitlist", localizedHandler(waitlistHandler(waitlistTmpl)))
	mux.HandleFunc("GET /{lang}/cancel", localizedHandler(cancelFormHandler(cancelTmpl)))
	mux.HandleFunc("POST /{lang}/cancel", localizedHandler(cancelHandler(db, mailer, cfg)))
	mux.HandleFunc("GET /organizer", organizerHandler(db, organizerTmpl, cfg))
	mux.HandleFunc("GET /organizer/login", organizerLoginHandler(organizerLoginTmpl))
	mux.HandleFunc("POST /organizer/login", organizerLoginPostHandler(cfg, organizerLoginTmpl))
	mux.HandleFunc("GET /health", healthHandler)
	return mux, nil
}

func rootRedirectHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := parseAcceptLanguage(r.Header.Get("Accept-Language"))
		http.Redirect(w, r, "/"+lang+"/", http.StatusFound)
	}
}

func localizedHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		if lang != "en" && lang != "de" {
			rest := strings.TrimPrefix(r.URL.Path, "/"+lang)
			target := "/en" + rest
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		h(w, r)
	}
}

type seatCounts struct {
	Lang            string
	DraftSeatsLeft  int
	SealedSeatsLeft int
}

func indexHandler(db *sql.DB, tmpl *template.Template, draftCap, sealedCap int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		draftLeft, err := seatsLeft(db, "draft", draftCap)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}
		sealedLeft, err := seatsLeft(db, "sealed", sealedCap)
		if err != nil {
			writeText(w, http.StatusInternalServerError, T(lang, "msg_server_error"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, seatCounts{Lang: lang, DraftSeatsLeft: draftLeft, SealedSeatsLeft: sealedLeft}); err != nil {
			log.Printf("index template execute: %v", err)
		}
	}
}

func seatsLeft(db *sql.DB, format string, cap int) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND status='confirmed'", format).Scan(&count)
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
	Lang string
}

func waitlistHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, waitlistData{Lang: lang}); err != nil {
			log.Printf("waitlist template execute: %v", err)
		}
	}
}

type cancelData struct {
	Lang string
}

func cancelFormHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, cancelData{Lang: lang}); err != nil {
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

		cap := cfg.draftCap
		amount := 15
		if format == "sealed" {
			cap = cfg.sealedCap
			amount = 30
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
		err = tx.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND status='confirmed'", format).Scan(&confirmedCount)
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
			"INSERT INTO submissions (email, name, format, mailing_list, created_at, status, lang) VALUES (?, ?, ?, ?, ?, ?, ?)",
			email, name, format, mailingListInt, createdAt, status, lang,
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
			body = confirmationEmailBody(name, format, email, host, lang)
		} else {
			subject = T(lang, "subject_waitlist")
			body = T(lang, "body_waitlist", name, formatName(format), seatTotal(format, cap), host, lang)
		}

		if err := mailer.Send(email, subject, body); err != nil {
			log.Printf("failed to send %s email to %s: %v", status, email, err)
			amountLabel := fmt.Sprintf("€%d", amount)
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
			http.Redirect(w, r, "/"+lang+"/pay?email="+url.QueryEscape(email), http.StatusFound)
		} else {
			http.Redirect(w, r, "/"+lang+"/waitlist", http.StatusFound)
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

func confirmationEmailBody(name, format, email, host, lang string) string {
	return T(lang, "body_confirmed", name, formatName(format), host, lang, url.QueryEscape(email), host, lang)
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

// organizerPageData wraps the submission rows with per-format seat counts
// shown next to the export button. Counted covers confirmed plus waitlist
// (cancelled rows excluded); Capacity is the configured seat cap.
type organizerPageData struct {
	Rows           []organizerRow
	DraftCount     int
	DraftCapacity  int
	SealedCount    int
	SealedCapacity int
}

// signupCount returns the number of confirmed plus waitlist submissions for
// the given format (cancelled rows excluded).
func signupCount(db *sql.DB, format string) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND status IN ('confirmed','waitlist')", format).Scan(&count)
	return count, err
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

func organizerHandler(db *sql.DB, tmpl *template.Template, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r, cfg) {
			http.Redirect(w, r, "/organizer/login", http.StatusFound)
			return
		}

		rows, err := db.Query(`SELECT id, email, name, format, mailing_list, created_at, payment, status, lang
			FROM submissions
			ORDER BY CASE status
				WHEN 'confirmed' THEN 0
				WHEN 'waitlist'  THEN 1
				WHEN 'cancelled' THEN 2
				ELSE 3
			END, name COLLATE NOCASE ASC`)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		defer rows.Close()

		if r.URL.Query().Get("export") == "csv" {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", "attachment; filename=\"submissions.csv\"")
			writer := csv.NewWriter(w)
			if err := writer.Write([]string{"id", "email", "name", "format", "mailing_list", "created_at", "payment", "status", "lang"}); err != nil {
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
					email,
					name,
					format,
					fmt.Sprintf("%d", mailingList),
					createdAt,
					payment,
					status,
					lang,
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

		draftCount, err := signupCount(db, "draft")
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		sealedCount, err := signupCount(db, "sealed")
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}

		data := organizerPageData{
			Rows:           organizerRows,
			DraftCount:     draftCount,
			DraftCapacity:  cfg.draftCap,
			SealedCount:    sealedCount,
			SealedCapacity: cfg.sealedCap,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("organizer template execute: %v", err)
		}
	}
}

func cancelHandler(db *sql.DB, mailer Mailer, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang")

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
		err := db.QueryRow("SELECT id, name, format, status FROM submissions WHERE email=?", email).Scan(&id, &name, &format, &status)
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
			err = tx.QueryRow("SELECT id, email, name, lang FROM submissions WHERE format=? AND status='waitlist' ORDER BY created_at ASC LIMIT 1", format).Scan(&promotedID, &promotedEmail, &promotedName, &promotedLang)
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
			amount := 15
			if format == "sealed" {
				amount = 30
			}

			if promoted {
				subject := T(promotedLang, "subject_confirmed")
				body := confirmationEmailBody(promotedName, format, promotedEmail, host, promotedLang)
				if err := mailer.Send(promotedEmail, subject, body); err != nil {
					log.Printf("failed to send promotion email to %s: %v", promotedEmail, err)
					failSubject := fmt.Sprintf("Failed to send payment mail to %s", promotedEmail)
					failBody := fmt.Sprintf("Timestamp: %s\nName: %s\nEmail: %s\nFormat: %s\nAmount: €%d\nError: %v",
						time.Now().UTC().Format(time.RFC3339), promotedName, promotedEmail, format, amount, err)
					if notifyErr := mailer.Send(cfg.organizerEmail, failSubject, failBody); notifyErr != nil {
						log.Printf("failed to send failure notification: %v", notifyErr)
					}
				}
			}

			notifySubject := fmt.Sprintf("Cancelled: %s", email)
			var notifyBody string
			if promoted {
				notifyBody = fmt.Sprintf("%s (%s) cancelled their %s spot. The seat went to %s (%s).", name, email, format, promotedName, promotedEmail)
			} else {
				notifyBody = fmt.Sprintf("%s (%s) cancelled their %s spot. No one is on the waitlist for %s.", name, email, format, format)
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
			body := fmt.Sprintf("%s (%s) cancelled their %s waitlist spot.", name, email, format)
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
	Email         string
	Format        string
	Amount        int
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
		email := strings.TrimSpace(r.URL.Query().Get("email"))
		if email == "" || !strings.Contains(email, "@") || hasNewline(email) {
			writeText(w, http.StatusOK, T(lang, "msg_not_available"))
			return
		}

		var id int
		var format, payment, status string
		err := db.QueryRow("SELECT id, format, payment, status FROM submissions WHERE email=?", email).Scan(&id, &format, &payment, &status)
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

		amount := 15
		if format == "sealed" {
			amount = 30
		}
		weroLinkWithAmount := fmt.Sprintf("%s?a=%d00&c=EUR", cfg.weroLink, amount)

		reference := fmt.Sprintf("Prerelease id %d", id)
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

		var id int
		var payment, status string
		err := db.QueryRow("SELECT id, payment, status FROM submissions WHERE email=?", email).Scan(&id, &payment, &status)
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
			http.Redirect(w, r, "/"+lang+"/pay?email="+url.QueryEscape(email), http.StatusFound)
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

		http.Redirect(w, r, "/"+lang+"/pay?email="+url.QueryEscape(email), http.StatusFound)
	}
}

func epcPayload(bic, recipient, iban string, amount int, reference string) string {
	amt := strings.Replace(fmt.Sprintf("%.2f", float64(amount)), ".", ",", 1)
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
