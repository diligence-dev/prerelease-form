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
		mailing_list INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		paid INTEGER NOT NULL DEFAULT 0,
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
		iban:           "DE1234567890",
		organizerEmail: "organizer@example.com",
		adminToken:     "secret-token",
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
	if loc == nil || loc.Path != "/thanks" {
		t.Fatalf("redirect = %v, want /thanks", loc)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if mailer.sends[0].to != "test@example.com" {
		t.Errorf("to = %q, want test@example.com", mailer.sends[0].to)
	}
	if !strings.Contains(mailer.sends[0].subject, "Payment details") {
		t.Errorf("subject missing Payment details: %q", mailer.sends[0].subject)
	}
	for _, want := range []string{"€15", "wero@example.com", "DE1234567890", "/cancel"} {
		if !strings.Contains(mailer.sends[0].body, want) {
			t.Errorf("body missing %q", want)
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
	if !strings.Contains(mailer.sends[0].body, "€30") {
		t.Errorf("body missing €30: %q", mailer.sends[0].body)
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
		{"missing mailing_list", func(v url.Values) { v.Del("mailing_list") }, "mailing"},
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
	if rows[0][len(rows[0])-1] != "status" {
		t.Errorf("last header = %q, want status", rows[0][len(rows[0])-1])
	}
	if rows[1][len(rows[1])-1] != "confirmed" {
		t.Errorf("last value = %q, want confirmed", rows[1][len(rows[1])-1])
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

func TestThanksPage(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/thanks")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "email with payment details") {
		t.Errorf("body missing expected text: %q", string(body))
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
	for _, bad := range []string{"€15", "IBAN"} {
		if strings.Contains(mailer.sends[0].body, bad) {
			t.Errorf("body contains %q", bad)
		}
	}
	if !strings.Contains(mailer.sends[0].body, "/cancel") {
		t.Errorf("body missing /cancel")
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

	var status string
	err := db.QueryRow("SELECT status FROM submissions WHERE email=?", "waitsealed@example.com").Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "waitlist" {
		t.Fatalf("status = %q, want waitlist", status)
	}
	for _, bad := range []string{"€30", "IBAN"} {
		if strings.Contains(mailer.sends[0].body, bad) {
			t.Errorf("body contains %q", bad)
		}
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
		if s.to == "waiter@example.com" && strings.Contains(s.subject, "Payment details") {
			promotionFound = true
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
