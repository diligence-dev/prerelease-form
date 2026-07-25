package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qrcode "github.com/yeqown/go-qrcode/v2"
	_ "modernc.org/sqlite"
)

type fakeMailer struct {
	sends []sent
	fail  map[int]bool
}

type sent struct {
	to      string
	subject string
	body    string
}

func (f *fakeMailer) Send(to, subject, body string) error {
	idx := len(f.sends)
	f.sends = append(f.sends, sent{to: to, subject: subject, body: body})
	if f.fail[idx] {
		return fmt.Errorf("fake send error")
	}
	return nil
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS submissions (
		id INTEGER PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		format TEXT NOT NULL,
		mailing_list INTEGER DEFAULT 0,
		created_at TEXT NOT NULL,
		payment TEXT NOT NULL DEFAULT 'unknown',
		status TEXT NOT NULL DEFAULT 'confirmed'
	)`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustInsert(t *testing.T, db *sql.DB, email, name, format, status string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status) VALUES (?, ?, ?, ?, ?, ?)`,
		email, name, format, 0, time.Now().UTC().Format(time.RFC3339), status)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func testConfig() config {
	return config{
		weroEmail:      "wero@example.com",
		weroLink:       "https://wero.example.com/pay",
		iban:           "DE1234567890",
		ibanRecipient:  "Test Recipient",
		bic:            "GENODEM1GLS",
		organizerEmail: "organizer@example.com",
		adminToken:     "secret-token",
		draftCap:       24,
		sealedCap:      8,
	}
}

func setupTestServer(t *testing.T, db *sql.DB, mailer Mailer) *httptest.Server {
	t.Helper()
	handler, err := setupHandlers(db, mailer, testConfig())
	if err != nil {
		t.Fatalf("setup handlers: %v", err)
	}
	return httptest.NewServer(handler)
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postFormNoRedirect(t *testing.T, client *http.Client, url string, form url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(url, form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func TestGetIndex(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"Draft", "Sealed"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestSubmitValidDraft(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "test@example.com")
	form.Set("name", "Test User")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/pay") {
		t.Fatalf("redirect = %v, want /pay?email=...", loc)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if mailer.sends[0].to != "test@example.com" {
		t.Errorf("to = %q, want test@example.com", mailer.sends[0].to)
	}
	if !strings.Contains(mailer.sends[0].subject, "signed up") {
		t.Errorf("subject missing 'signed up': %q", mailer.sends[0].subject)
	}
	if !strings.Contains(mailer.sends[0].body, "/pay?email=") || !strings.Contains(mailer.sends[0].body, "/cancel") {
		t.Errorf("body missing /pay?email= or /cancel: %q", mailer.sends[0].body)
	}
	for _, bad := range []string{"15", "wero@example.com", "DE1234567890"} {
		if strings.Contains(mailer.sends[0].body, bad) {
			t.Errorf("body contains %q", bad)
		}
	}
}

func TestSubmitValidSealed(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "sealed@example.com")
	form.Set("name", "Sealed User")
	form.Set("format", "sealed")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "no")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if strings.Contains(mailer.sends[0].body, "30") {
		t.Errorf("body should not contain amount: %q", mailer.sends[0].body)
	}
}

func TestSubmitValidationErrors(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	base := url.Values{}
	base.Set("email", "valid@example.com")
	base.Set("name", "Valid Name")
	base.Set("format", "draft")
	base.Set("cancellation_ack", "on")
	base.Set("data_consent", "on")
	base.Set("mailing_list", "yes")

	cases := []struct {
		name  string
		mod   func(url.Values)
		field string
	}{
		{"missing email", func(v url.Values) { v.Del("email") }, "email"},
		{"missing name", func(v url.Values) { v.Del("name") }, "name"},
		{"missing format", func(v url.Values) { v.Del("format") }, "format"},
		{"missing cancellation_ack", func(v url.Values) { v.Del("cancellation_ack") }, "cancellation"},
		{"missing data_consent", func(v url.Values) { v.Del("data_consent") }, "consent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{}
			for k, vv := range base {
				form[k] = vv
			}
			tc.mod(form)
			client := noRedirectClient()
			resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestSubmitDuplicateEmail(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "dup@example.com")
	form.Set("name", "Dup")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")

	resp, _ := http.PostForm(server.URL+"/submit", form)
	resp.Body.Close()
	client := noRedirectClient()
	resp = postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "already registered") {
		t.Errorf("body missing already registered: %q", string(body))
	}
}

func TestSubmissionsCSVMissingToken(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/submissions.csv")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSubmissionsCSVWrongToken(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/submissions.csv?token=wrong")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSubmissionsCSVCorrectToken(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "csv@example.com")
	form.Set("name", "CSV User")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	http.PostForm(server.URL+"/submit", form)

	resp, err := http.Get(server.URL + "/submissions.csv?token=secret-token")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv" {
		t.Errorf("content-type = %q, want text/csv", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	reader := csv.NewReader(bytes.NewReader(body))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("csv read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	foundPayment := false
	foundUnknown := false
	for i, col := range rows[0] {
		if col == "payment" {
			foundPayment = true
			if rows[1][i] == "unknown" {
				foundUnknown = true
			}
		}
	}
	if !foundPayment {
		t.Errorf("payment column not found in header: %v", rows[0])
	}
	if !foundUnknown {
		t.Errorf("payment value not 'unknown': %v", rows[1])
	}
}

func TestHealth(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", string(body))
	}
}

func TestSubmitMailerFailNotifies(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{fail: map[int]bool{0: true}}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "fail@example.com")
	form.Set("name", "Fail User")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/pay") {
		t.Errorf("redirect = %v, want /pay?email=...", loc)
	}
	if len(mailer.sends) != 2 {
		t.Fatalf("sends = %d, want 2", len(mailer.sends))
	}
	if mailer.sends[1].to != "organizer@example.com" {
		t.Errorf("notify to = %q, want organizer@example.com", mailer.sends[1].to)
	}
}

func TestSubmitMailerBothFailStillRedirects(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{fail: map[int]bool{0: true, 1: true}}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "bothfail@example.com")
	form.Set("name", "Both Fail")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/pay") {
		t.Errorf("redirect = %v, want /pay?email=...", loc)
	}
}

func TestDraftWaitlist(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 24; i++ {
		mustInsert(t, db, fmt.Sprintf("draft%d@example.com", i), fmt.Sprintf("Draft %d", i), "draft", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "waitdraft@example.com")
	form.Set("name", "Wait Draft")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/waitlist" {
		t.Fatalf("redirect = %v, want /waitlist", loc)
	}

	var status string
	err := db.QueryRow("SELECT status FROM submissions WHERE email=?", "waitdraft@example.com").Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "waitlist" {
		t.Fatalf("status = %q, want waitlist", status)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if !strings.Contains(mailer.sends[0].body, "waitlist") {
		t.Errorf("body missing waitlist: %q", mailer.sends[0].body)
	}
}

func TestSealedWaitlist(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 8; i++ {
		mustInsert(t, db, fmt.Sprintf("sealed%d@example.com", i), fmt.Sprintf("Sealed %d", i), "sealed", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "waitsealed@example.com")
	form.Set("name", "Wait Sealed")
	form.Set("format", "sealed")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/waitlist" {
		t.Fatalf("redirect = %v, want /waitlist", loc)
	}

	var status string
	err := db.QueryRow("SELECT status FROM submissions WHERE email=?", "waitsealed@example.com").Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "waitlist" {
		t.Fatalf("status = %q, want waitlist", status)
	}
}

func TestCustomCapacities(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	cfg := testConfig()
	cfg.draftCap = 3
	cfg.sealedCap = 2
	handler, err := setupHandlers(db, mailer, cfg)
	if err != nil {
		t.Fatalf("setup handlers: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	for i := 0; i < 3; i++ {
		mustInsert(t, db, fmt.Sprintf("draft%d@example.com", i), fmt.Sprintf("Draft %d", i), "draft", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "waitdraft@example.com")
	form.Set("name", "Wait Draft")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/waitlist" {
		t.Fatalf("redirect = %v, want /waitlist", loc)
	}

	var status string
	err = db.QueryRow("SELECT status FROM submissions WHERE email=?", "waitdraft@example.com").Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "waitlist" {
		t.Fatalf("status = %q, want waitlist", status)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if !strings.Contains(mailer.sends[0].body, "3 draft") {
		t.Errorf("body missing '3 draft': %q", mailer.sends[0].body)
	}
}

func TestSeatCountsAfterSubmissions(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 3; i++ {
		mustInsert(t, db, fmt.Sprintf("draft%d@example.com", i), fmt.Sprintf("Draft %d", i), "draft", "confirmed")
	}

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "21") {
		t.Errorf("body missing 21: %q", string(body))
	}
	if !strings.Contains(string(body), "8") {
		t.Errorf("body missing 8: %q", string(body))
	}
}

func TestCancelPage(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/cancel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`<input`, `action="/cancel"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestCancelConfirmedPromotesWaitlist(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "canceler@example.com", "Canceler", "draft", "confirmed")
	mustInsert(t, db, "waiter@example.com", "Waiter", "draft", "waitlist")

	form := url.Values{}
	form.Set("email", "canceler@example.com")
	resp, err := http.PostForm(server.URL+"/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "registration has been cancelled") {
		t.Errorf("body missing cancellation text: %q", string(body))
	}

	var cancelStatus, waitStatus string
	db.QueryRow("SELECT status FROM submissions WHERE email=?", "canceler@example.com").Scan(&cancelStatus)
	db.QueryRow("SELECT status FROM submissions WHERE email=?", "waiter@example.com").Scan(&waitStatus)
	if cancelStatus != "cancelled" {
		t.Errorf("canceler status = %q, want cancelled", cancelStatus)
	}
	if waitStatus != "confirmed" {
		t.Errorf("waiter status = %q, want confirmed", waitStatus)
	}

	var promotionFound, organizerFound bool
	for _, s := range mailer.sends {
		if s.to == "waiter@example.com" && strings.Contains(s.subject, "signed up") {
			promotionFound = true
			if !strings.Contains(s.body, "/pay?email=") {
				t.Errorf("promotion email missing /pay?email=: %s", s.body)
			}
		}
		if s.to == "organizer@example.com" && strings.Contains(s.body, "Canceler") && strings.Contains(s.body, "Waiter") {
			organizerFound = true
		}
	}
	if !promotionFound {
		t.Errorf("promotion email not found")
	}
	if !organizerFound {
		t.Errorf("organizer notification not found")
	}
}

func TestCancelConfirmedNoWaitlist(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "canceler2@example.com", "Canceler2", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "canceler2@example.com")
	resp, err := http.PostForm(server.URL+"/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var status string
	db.QueryRow("SELECT status FROM submissions WHERE email=?", "canceler2@example.com").Scan(&status)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}

	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if !strings.Contains(mailer.sends[0].body, "No one is on the waitlist") {
		t.Errorf("body missing waitlist message: %q", mailer.sends[0].body)
	}
}

func TestCancelWaitlist(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "waitcancel@example.com", "Wait Cancel", "sealed", "waitlist")

	form := url.Values{}
	form.Set("email", "waitcancel@example.com")
	resp, err := http.PostForm(server.URL+"/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var status string
	db.QueryRow("SELECT status FROM submissions WHERE email=?", "waitcancel@example.com").Scan(&status)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if mailer.sends[0].to != "organizer@example.com" {
		t.Errorf("notify to = %q, want organizer@example.com", mailer.sends[0].to)
	}
}

func TestCancelUnknownEmail(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "unknown@example.com")
	resp, err := http.PostForm(server.URL+"/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "No registration found") {
		t.Errorf("body missing no registration text: %q", string(body))
	}
}

func TestCancelAlreadyCancelled(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "already@example.com", "Already", "draft", "cancelled")

	form := url.Values{}
	form.Set("email", "already@example.com")
	resp, err := http.PostForm(server.URL+"/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "already cancelled") {
		t.Errorf("body missing already cancelled: %q", string(body))
	}
}

func TestSubmissionsCSVConstantTimeCompare(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	// Constant-time compare should be used, but we verify behavior through wrong token test above.
	// Ensure that a token of correct length but different content is rejected.
	resp, err := http.Get(server.URL + "/submissions.csv?token=" + strings.Repeat("x", len("secret-token")))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSeatsLeftClampedAtZero(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	// More confirmed than cap should not cause negative display.
	for i := 0; i < 30; i++ {
		mustInsert(t, db, fmt.Sprintf("overdraft%d@example.com", i), fmt.Sprintf("Over %d", i), "draft", "confirmed")
	}

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// We expect label to show 0 left for draft.
	if strings.Contains(string(body), "-6") {
		t.Errorf("body contains negative seats: %q", string(body))
	}
}

func TestSubmitRejectsCRLFInName(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for _, bad := range []string{"Name\r\nBcc: evil@x.com", "Line\nBreak", "Carriage\rReturn"} {
		form := url.Values{}
		form.Set("email", "crlf@example.com")
		form.Set("name", bad)
		form.Set("format", "draft")
		form.Set("cancellation_ack", "on")
		form.Set("data_consent", "on")
		form.Set("mailing_list", "yes")
		client := noRedirectClient()
		resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want %d", bad, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestSubmitRejectsCRLFInEmail(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "attacker@example.com\r\nBcc: victim@x.com")
	form.Set("name", "Attacker")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestEPCPayload(t *testing.T) {
	got := epcPayload("GENODEM1GLS", "Test Recipient", "DE12345678901234567890", 15, 7)
	want := "BCD\r\n001\r\n1\r\nSCT\r\nGENODEM1GLS\r\nTest Recipient\r\nDE12345678901234567890\r\nEUR15,00\r\n\r\n\r\n7\r\n\r\n"
	if got != want {
		t.Errorf("epcPayload() = %q, want %q", got, want)
	}
}

func TestSVGWriter(t *testing.T) {
	const scale = 8
	svg, err := qrSVG("test", scale)
	if err != nil {
		t.Fatalf("qrSVG error: %v", err)
	}
	s := string(svg)
	if !strings.Contains(s, "<svg") || !strings.Contains(s, "</svg>") {
		t.Errorf("missing svg tags: %s", s)
	}
	if !strings.Contains(s, `<rect`) || !strings.Contains(s, `fill="#000"`) {
		t.Errorf("missing rect with fill: %s", s)
	}
	// Build the QR directly to learn its dimension, then assert the SVG's
	// width/height equal dim*scale (the svgWriter contract).
	qrc, err := qrcode.New("test")
	if err != nil {
		t.Fatalf("qrcode.New: %v", err)
	}
	dim := qrc.Dimension()
	want := fmt.Sprintf(`width="%d" height="%d"`, dim*scale, dim*scale)
	if !strings.Contains(s, want) {
		t.Errorf("svg missing %q: %s", want, s)
	}
}

func TestPayPageConfirmed(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "draftuser@example.com", "Draft User", "draft", "confirmed")

	resp, err := http.Get(server.URL + "/pay?email=draftuser@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"Wero", "IBAN", "Cash", "€15", "<svg"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestPayPageSealed(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "sealeduser@example.com", "Sealed User", "sealed", "confirmed")

	resp, err := http.Get(server.URL + "/pay?email=sealeduser@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "€30") {
		t.Errorf("body missing €30: %s", string(body))
	}
}

func TestPayPageUnknownEmail(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/pay?email=nobody@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not available") {
		t.Errorf("body missing 'not available': %s", string(body))
	}
}

func TestPayPageWaitlist(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "waitlist@example.com", "Waitlist User", "draft", "waitlist")

	resp, err := http.Get(server.URL + "/pay?email=waitlist@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not available") {
		t.Errorf("body missing 'not available': %s", string(body))
	}
}

func TestPayPageCancelled(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "cancelled@example.com", "Cancelled User", "draft", "cancelled")

	resp, err := http.Get(server.URL + "/pay?email=cancelled@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not available") {
		t.Errorf("body missing 'not available': %s", string(body))
	}
}

func TestPayPageAlreadyPaid(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, payment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"paid@example.com", "Paid User", "draft", 0, time.Now().UTC().Format(time.RFC3339), "confirmed", "paid")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := http.Get(server.URL + "/pay?email=paid@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "Done, I've paid") {
		t.Errorf("body should not contain payment options: %s", string(body))
	}
	for _, opt := range []string{"Pay via Wero", "Pay via IBAN", "Pay Cash"} {
		if strings.Contains(string(body), opt) {
			t.Errorf("body should not contain option %q: %s", opt, string(body))
		}
	}
	if !strings.Contains(string(body), "recorded your payment") {
		t.Errorf("body should show confirmation: %s", string(body))
	}
}

func TestPayPageCashMarked(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, payment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"cash@example.com", "Cash User", "draft", 0, time.Now().UTC().Format(time.RFC3339), "confirmed", "cash")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := http.Get(server.URL + "/pay?email=cash@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pay cash at the event") {
		t.Errorf("body should show cash confirmation: %s", string(body))
	}
}

func TestPayMarkPaidWero(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "weropay@example.com", "Wero Pay", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "weropay@example.com")
	form.Set("method", "wero")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/pay", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if !strings.HasPrefix(loc.Path, "/pay") {
		t.Errorf("redirect = %v, want /pay", loc)
	}

	var payment string
	err := db.QueryRow("SELECT payment FROM submissions WHERE email=?", "weropay@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "paid" {
		t.Errorf("payment = %q, want paid", payment)
	}
}

func TestPayMarkPaidIBAN(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "ibanpay@example.com", "IBAN Pay", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "ibanpay@example.com")
	form.Set("method", "iban")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/pay", form)
	defer resp.Body.Close()

	var payment string
	err := db.QueryRow("SELECT payment FROM submissions WHERE email=?", "ibanpay@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "paid" {
		t.Errorf("payment = %q, want paid", payment)
	}
}

func TestPayMarkCash(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "cashpay@example.com", "Cash Pay", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "cashpay@example.com")
	form.Set("method", "cash")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/pay", form)
	defer resp.Body.Close()

	var payment string
	err := db.QueryRow("SELECT payment FROM submissions WHERE email=?", "cashpay@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "cash" {
		t.Errorf("payment = %q, want cash", payment)
	}
}

func TestPayLockedAfterMark(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, payment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"locked@example.com", "Locked User", "draft", 0, time.Now().UTC().Format(time.RFC3339), "confirmed", "paid")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	form := url.Values{}
	form.Set("email", "locked@example.com")
	form.Set("method", "cash")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/pay", form)
	defer resp.Body.Close()

	var payment string
	err = db.QueryRow("SELECT payment FROM submissions WHERE email=?", "locked@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "paid" {
		t.Errorf("payment changed to %q, should stay paid", payment)
	}
}

func TestPayInvalidMethod(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, "invalid@example.com", "Invalid", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "invalid@example.com")
	form.Set("method", "bogus")
	resp, err := http.PostForm(server.URL+"/pay", form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestPayInvalidEmail(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "")
	form.Set("method", "wero")
	resp, err := http.PostForm(server.URL+"/pay", form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}
