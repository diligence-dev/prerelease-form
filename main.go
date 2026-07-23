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

var indexTmpl *template.Template

type Mailer interface {
	Send(to, subject, body string) error
}

type smtpMailer struct {
	host, port, user, password, from string
}

func (m smtpMailer) Send(to, subject, body string) error {
	msg := []byte("To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\n" +
		body + "\r\n")
	addr := m.host + ":" + m.port
	auth := smtp.PlainAuth("", m.user, m.password, m.host)
	return smtp.SendMail(addr, auth, m.from, []string{to}, msg)
}

func main() {
	dataPath := getenv("DATA_PATH", "data.db")
	db, err := sql.Open("sqlite", dataPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		log.Fatalf("init schema: %v", err)
	}

	tmpl, err := template.ParseFS(embeddedFiles, "index.html")
	if err != nil {
		log.Fatalf("parse template: %v", err)
	}
	indexTmpl = tmpl

	password := os.Getenv("SMTP_PASSWORD")
	if password == "" {
		log.Fatalf("SMTP_PASSWORD not set")
	}
	from := getenv("SMTP_FROM", "diligence.bot@web.de")
	mailer := smtpMailer{
		host:     "smtp.web.de",
		port:     "587",
		user:     "diligence.bot@web.de",
		password: password,
		from:     from,
	}

	handler, err := setupHandlers(db, mailer)
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
		mailing_list INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		paid INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'confirmed'
	)`)
	return err
}

func setupHandlers(db *sql.DB, mailer Mailer) (http.Handler, error) {
	tmpl, err := template.ParseFS(embeddedFiles, "index.html")
	if err != nil {
		return nil, err
	}
	indexTmpl = tmpl

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", indexHandler(db))
	mux.HandleFunc("POST /submit", submitHandler(db, mailer))
	mux.HandleFunc("GET /thanks", staticHandler("thanks.html"))
	mux.HandleFunc("GET /cancel", staticHandler("cancel.html"))
	mux.HandleFunc("POST /cancel", cancelHandler(db, mailer))
	mux.HandleFunc("GET /submissions.csv", csvHandler(db))
	mux.HandleFunc("GET /health", healthHandler)
	return mux, nil
}

type seatCounts struct {
	DraftSeatsLeft  int
	SealedSeatsLeft int
}

func indexHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		draftLeft, err := seatsLeft(db, "draft", draftCap)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		sealedLeft, err := seatsLeft(db, "sealed", sealedCap)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTmpl.Execute(w, seatCounts{DraftSeatsLeft: draftLeft, SealedSeatsLeft: sealedLeft})
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
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	}
}

func submitHandler(db *sql.DB, mailer Mailer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		name := strings.TrimSpace(r.FormValue("name"))
		format := r.FormValue("format")
		cancellationAck := r.FormValue("cancellation_ack")
		dataConsent := r.FormValue("data_consent")
		mailingList := r.FormValue("mailing_list")

		switch {
		case email == "" || !strings.Contains(email, "@"):
			http.Error(w, "Invalid email.", http.StatusBadRequest)
			return
		case name == "" || len(name) > 200:
			http.Error(w, "Invalid name.", http.StatusBadRequest)
			return
		case format != "draft" && format != "sealed":
			http.Error(w, "Invalid format.", http.StatusBadRequest)
			return
		case cancellationAck != "on":
			http.Error(w, "Cancellation acknowledgement required.", http.StatusBadRequest)
			return
		case dataConsent != "on":
			http.Error(w, "Data consent required.", http.StatusBadRequest)
			return
		case mailingList != "yes" && mailingList != "no":
			http.Error(w, "Mailing list choice required.", http.StatusBadRequest)
			return
		}

		cap := draftCap
		amount := 15
		if format == "sealed" {
			cap = sealedCap
			amount = 30
		}

		mailingInt := 0
		if mailingList == "yes" {
			mailingInt = 1
		}

		tx, err := db.Begin()
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var confirmedCount int
		err = tx.QueryRow("SELECT COUNT(*) FROM submissions WHERE format=? AND status='confirmed'", format).Scan(&confirmedCount)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		status := "confirmed"
		if confirmedCount >= cap {
			status = "waitlist"
		}

		createdAt := time.Now().UTC().Format(time.RFC3339)
		_, err = tx.Exec(
			"INSERT INTO submissions (email, name, format, mailing_list, created_at, status) VALUES (?, ?, ?, ?, ?, ?)",
			email, name, format, mailingInt, createdAt, status,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				http.Error(w, "You have already registered with this email.", http.StatusConflict)
				return
			}
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		weroEmail := os.Getenv("WERO_EMAIL")
		iban := os.Getenv("IBAN")
		host := "https://" + r.Host
		failNotify := getenv("SMTP_FAIL_NOTIFY", "diligence.dev@web.de")

		var subject, body string
		if status == "confirmed" {
			subject = "MTG Prerelease – Payment details"
			body = fmt.Sprintf(`Hi %s,

Thanks for signing up for %s.
Please send €%d via Wero to %s (IBAN: %s).
Use "%s" as reference.
We'll mark you paid once we receive the Wero notification.

If you can no longer attend, cancel at %s/cancel using your email address.
`, name, formatName(format), amount, weroEmail, iban, name, host)
		} else {
			subject = "MTG Prerelease – You're on the waitlist"
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
			if notifyErr := mailer.Send(failNotify, failSubject, failBody); notifyErr != nil {
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

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func csvHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		adminToken := os.Getenv("ADMIN_TOKEN")
		if adminToken == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminToken)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		rows, err := db.Query("SELECT id, email, name, format, mailing_list, created_at, paid, status FROM submissions ORDER BY created_at DESC")
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=\"submissions.csv\"")
		writer := csv.NewWriter(w)
		writer.Write([]string{"id", "email", "name", "format", "mailing_list", "created_at", "paid", "status"})
		for rows.Next() {
			var id, mailingList, paid int
			var email, name, format, createdAt, status string
			if err := rows.Scan(&id, &email, &name, &format, &mailingList, &createdAt, &paid, &status); err != nil {
				return
			}
			writer.Write([]string{
				fmt.Sprintf("%d", id),
				email,
				name,
				format,
				fmt.Sprintf("%d", mailingList),
				createdAt,
				fmt.Sprintf("%d", paid),
				status,
			})
		}
		writer.Flush()
	}
}

func cancelHandler(db *sql.DB, mailer Mailer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		if email == "" || !strings.Contains(email, "@") {
			http.Error(w, "Invalid email.", http.StatusBadRequest)
			return
		}

		var id int
		var name, format, status string
		err := db.QueryRow("SELECT id, name, format, status FROM submissions WHERE email=?", email).Scan(&id, &name, &format, &status)
		if err == sql.ErrNoRows {
			http.Error(w, "No registration found for that email.", http.StatusOK)
			return
		}
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		if status == "cancelled" {
			http.Error(w, "Your registration was already cancelled.", http.StatusOK)
			return
		}

		failNotify := getenv("SMTP_FAIL_NOTIFY", "diligence.dev@web.de")
		host := "https://" + r.Host
		weroEmail := os.Getenv("WERO_EMAIL")
		iban := os.Getenv("IBAN")

		if status == "confirmed" {
			tx, err := db.Begin()
			if err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			defer tx.Rollback()

			_, err = tx.Exec("UPDATE submissions SET status='cancelled' WHERE id=?", id)
			if err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}

			var promotedID int
			var promotedEmail, promotedName string
			err = tx.QueryRow("SELECT id, email, name FROM submissions WHERE format=? AND status='waitlist' ORDER BY created_at ASC LIMIT 1", format).Scan(&promotedID, &promotedEmail, &promotedName)
			promoted := err == nil
			if promoted {
				_, err = tx.Exec("UPDATE submissions SET status='confirmed' WHERE id=?", promotedID)
				if err != nil {
					http.Error(w, "server error", http.StatusInternalServerError)
					return
				}
			}

			if err := tx.Commit(); err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}

			if promoted {
				amount := 15
				if format == "sealed" {
					amount = 30
				}
				subject := "MTG Prerelease – Payment details"
				body := fmt.Sprintf(`Hi %s,

Thanks for signing up for %s.
Please send €%d via Wero to %s (IBAN: %s).
Use "%s" as reference.
We'll mark you paid once we receive the Wero notification.

If you can no longer attend, cancel at %s/cancel using your email address.
`, promotedName, formatName(format), amount, weroEmail, iban, promotedName, host)
				if err := mailer.Send(promotedEmail, subject, body); err != nil {
					log.Printf("failed to send promotion email to %s: %v", promotedEmail, err)
					failSubject := fmt.Sprintf("Failed to send payment mail to %s", promotedEmail)
					failBody := fmt.Sprintf("Timestamp: %s\nName: %s\nEmail: %s\nFormat: %s\nAmount: €%d\nError: %v",
						time.Now().UTC().Format(time.RFC3339), promotedName, promotedEmail, format, amount, err)
					if notifyErr := mailer.Send(failNotify, failSubject, failBody); notifyErr != nil {
						log.Printf("failed to send failure notification: %v", notifyErr)
					}
				}
			}

			var notifySubject, notifyBody string
			if promoted {
				notifySubject = fmt.Sprintf("Cancelled: %s", email)
				notifyBody = fmt.Sprintf("%s (%s) cancelled their %s spot. The seat went to %s (%s).", name, email, format, promotedName, promotedEmail)
			} else {
				notifySubject = fmt.Sprintf("Cancelled: %s", email)
				notifyBody = fmt.Sprintf("%s (%s) cancelled their %s spot. No one is on the waitlist for %s.", name, email, format, format)
			}
			if err := mailer.Send(failNotify, notifySubject, notifyBody); err != nil {
				log.Printf("failed to send organizer notification: %v", err)
			}

			http.Error(w, "Your registration has been cancelled.", http.StatusOK)
			return
		}

		if status == "waitlist" {
			_, err := db.Exec("UPDATE submissions SET status='cancelled' WHERE id=?", id)
			if err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}

			subject := fmt.Sprintf("Cancelled waitlist: %s", email)
			body := fmt.Sprintf("%s (%s) cancelled their %s waitlist spot.", name, email, format)
			if err := mailer.Send(failNotify, subject, body); err != nil {
				log.Printf("failed to send organizer notification: %v", err)
			}

			http.Error(w, "Your waitlist spot has been cancelled.", http.StatusOK)
			return
		}

		http.Error(w, "server error", http.StatusInternalServerError)
	}
}
