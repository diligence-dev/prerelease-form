package main

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	draftCap  = 24
	sealedCap = 8
)

//go:embed index.html thanks.html cancel.html
var embeddedFiles embed.FS

type Mailer interface {
	Send(to, subject, body string) error
}

type smtpMailer struct {
	host, port, user, password, from string
}

func (m smtpMailer) Send(to, subject, body string) error {
	msg := []byte("From: " + m.from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\n" +
		body + "\r\n")
	addr := m.host + ":" + m.port
	auth := smtp.PlainAuth("", m.user, m.password, m.host)
	return smtp.SendMail(addr, auth, m.from, []string{to}, msg)
}

// config holds runtime configuration resolved once at startup so handlers
// never read os.Getenv directly and required values are validated up front.
type config struct {
	weroEmail      string
	weroLink       string
	iban           string
	ibanRecipient  string
	bic            string
	organizerEmail string
	adminToken     string
}

// loadConfig reads configuration from the environment and validates required
// values. Missing required values are fatal at startup so the server never
// boots into a state where payment/organizer emails would silently be empty.
func loadConfig() config {
	cfg := config{
		weroEmail:      os.Getenv("WERO_EMAIL"),
		weroLink:       os.Getenv("WERO_LINK"),
		iban:           os.Getenv("IBAN"),
		ibanRecipient:  os.Getenv("IBAN_RECIPIENT"),
		bic:            os.Getenv("BIC"),
		organizerEmail: getenv("ORGANIZER_EMAIL", "diligence.dev@web.de"),
		adminToken:     os.Getenv("ADMIN_TOKEN"),
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
	if cfg.adminToken == "" {
		log.Fatalf("ADMIN_TOKEN not set")
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
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS submissions (
		id INTEGER PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		format TEXT NOT NULL,
		mailing_list INTEGER DEFAULT 0,
		created_at TEXT NOT NULL,
		paid INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'confirmed'
	)`)
	return err
}

func setupHandlers(db *sql.DB, mailer Mailer, cfg config) (http.Handler, error) {
	tmpl, err := template.ParseFS(embeddedFiles, "index.html")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", indexHandler(db, tmpl))
	mux.HandleFunc("POST /submit", submitHandler(db, mailer, cfg))
	mux.HandleFunc("GET /thanks", staticHandler("thanks.html"))
	mux.HandleFunc("GET /cancel", staticHandler("cancel.html"))
	mux.HandleFunc("POST /cancel", cancelHandler(db, mailer, cfg))
	mux.HandleFunc("GET /submissions.csv", csvHandler(db, cfg))
	mux.HandleFunc("GET /health", healthHandler)
	return mux, nil
}

type seatCounts struct {
	DraftSeatsLeft  int
	SealedSeatsLeft int
}

func indexHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		draftLeft, err := seatsLeft(db, "draft", draftCap)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		sealedLeft, err := seatsLeft(db, "sealed", sealedCap)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, seatCounts{DraftSeatsLeft: draftLeft, SealedSeatsLeft: sealedLeft}); err != nil {
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

func staticHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := embeddedFiles.ReadFile(name)
		if err != nil {
			writeText(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
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
		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, "invalid form")
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
			writeText(w, http.StatusBadRequest, "Invalid email.")
			return
		case name == "" || len(name) > 200 || hasNewline(name):
			writeText(w, http.StatusBadRequest, "Invalid name.")
			return
		case format != "draft" && format != "sealed":
			writeText(w, http.StatusBadRequest, "Invalid format.")
			return
		case cancellationAck != "on":
			writeText(w, http.StatusBadRequest, "Cancellation acknowledgement required.")
			return
		case dataConsent != "on":
			writeText(w, http.StatusBadRequest, "Data consent required.")
			return
		}

		cap := draftCap
		amount := 15
		if format == "sealed" {
			cap = sealedCap
			amount = 30
		}

		mailingListInt := 0
		if mailingList == "yes" {
			mailingListInt = 1
		}

		tx, err := db.Begin()
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		defer tx.Rollback()

		var confirmedCount int
		err = tx.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND status='confirmed'", format).Scan(&confirmedCount)
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}

		status := "confirmed"
		if confirmedCount >= cap {
			status = "waitlist"
		}

		createdAt := time.Now().UTC().Format(time.RFC3339)
		_, err = tx.Exec(
			"INSERT INTO submissions (email, name, format, mailing_list, created_at, status) VALUES (?, ?, ?, ?, ?, ?)",
			email, name, format, mailingListInt, createdAt, status,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				writeText(w, http.StatusConflict, "You have already registered with this email.")
				return
			}
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}

		if err := tx.Commit(); err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}

		host := "https://" + r.Host

		var subject, body string
		if status == "confirmed" {
			subject = "Prerelease – Payment details"
			body = confirmationEmailBody(name, format, amount, cfg, host)
		} else {
			subject = "Prerelease – You're on the waitlist"
			body = fmt.Sprintf(`Hi %s,

Thanks for signing up for %s.
All %s seats are currently taken, so you've been added to the waitlist.
You'll receive another email with payment details as soon as a seat opens up for you.

If you no longer wish to be on the waitlist, cancel at %s/cancel using your email address.
`, name, formatName(format), seatTotal(format), host)
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

		http.Redirect(w, r, "/thanks", http.StatusFound)
	}
}

func formatName(format string) string {
	if format == "draft" {
		return "Draft"
	}
	return "Sealed"
}

func seatTotal(format string) string {
	if format == "draft" {
		return "24 draft"
	}
	return "8 sealed"
}

func confirmationEmailBody(name, format string, amount int, cfg config, host string) string {
	return fmt.Sprintf(`Hi %s,

you are signed up for the prerelease - you will be playing %s!
There are 3 options to pay your %d Euro:
- Wero to %s: %s
- IBAN: %s, recipient: %s, BIC: %s
- bring cash to the event (paying in advance is appreciated though)
If you can no longer attend, please cancel at %s/cancel.
Looking forward to seeing you at the event!
`, name, formatName(format), amount, cfg.weroEmail, cfg.weroLink, cfg.iban, cfg.ibanRecipient, cfg.bic, host)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func csvHandler(db *sql.DB, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.adminToken)) != 1 {
			writeText(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		rows, err := db.Query("SELECT id, email, name, format, mailing_list, created_at, paid, status FROM submissions ORDER BY created_at DESC")
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}
		defer rows.Close()

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=\"submissions.csv\"")
		writer := csv.NewWriter(w)
		if err := writer.Write([]string{"id", "email", "name", "format", "mailing_list", "created_at", "paid", "status"}); err != nil {
			log.Printf("csv write header: %v", err)
			return
		}
		for rows.Next() {
			var id, mailingList, paid int
			var email, name, format, createdAt, status string
			if err := rows.Scan(&id, &email, &name, &format, &mailingList, &createdAt, &paid, &status); err != nil {
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
				fmt.Sprintf("%d", paid),
				status,
			}); err != nil {
				log.Printf("csv write row: %v", err)
				return
			}
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			log.Printf("csv flush: %v", err)
		}
	}
}

func cancelHandler(db *sql.DB, mailer Mailer, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeText(w, http.StatusBadRequest, "invalid form")
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		if email == "" || !strings.Contains(email, "@") || hasNewline(email) {
			writeText(w, http.StatusBadRequest, "Invalid email.")
			return
		}

		var id int
		var name, format, status string
		err := db.QueryRow("SELECT id, name, format, status FROM submissions WHERE email=?", email).Scan(&id, &name, &format, &status)
		if err == sql.ErrNoRows {
			writeText(w, http.StatusOK, "No registration found for that email.")
			return
		}
		if err != nil {
			writeText(w, http.StatusInternalServerError, "server error")
			return
		}

		if status == "cancelled" {
			writeText(w, http.StatusOK, "Your registration was already cancelled.")
			return
		}

		host := "https://" + r.Host

		if status == "confirmed" {
			tx, err := db.Begin()
			if err != nil {
				writeText(w, http.StatusInternalServerError, "server error")
				return
			}
			defer tx.Rollback()

			_, err = tx.Exec("UPDATE submissions SET status='cancelled' WHERE id=?", id)
			if err != nil {
				writeText(w, http.StatusInternalServerError, "server error")
				return
			}

			var promotedID int
			var promotedEmail, promotedName string
			err = tx.QueryRow("SELECT id, email, name FROM submissions WHERE format=? AND status='waitlist' ORDER BY created_at ASC LIMIT 1", format).Scan(&promotedID, &promotedEmail, &promotedName)
			promoted := err == nil
			if promoted {
				_, err = tx.Exec("UPDATE submissions SET status='confirmed' WHERE id=?", promotedID)
				if err != nil {
					writeText(w, http.StatusInternalServerError, "server error")
					return
				}
			}

			if err := tx.Commit(); err != nil {
				writeText(w, http.StatusInternalServerError, "server error")
				return
			}
			if promoted {
				amount := 15
				if format == "sealed" {
					amount = 30
				}
				subject := "Prerelease – Payment details"
				body := confirmationEmailBody(promotedName, format, amount, cfg, host)
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

			writeText(w, http.StatusOK, "Your registration has been cancelled.")
			return
		}

		if status == "waitlist" {
			_, err := db.Exec("UPDATE submissions SET status='cancelled' WHERE id=?", id)
			if err != nil {
				writeText(w, http.StatusInternalServerError, "server error")
				return
			}

			subject := fmt.Sprintf("Cancelled waitlist: %s", email)
			body := fmt.Sprintf("%s (%s) cancelled their %s waitlist spot.", name, email, format)
			if err := mailer.Send(cfg.organizerEmail, subject, body); err != nil {
				log.Printf("failed to send organizer notification: %v", err)
			}

			writeText(w, http.StatusOK, "Your waitlist spot has been cancelled.")
			return
		}

		writeText(w, http.StatusInternalServerError, "server error")
	}
}
