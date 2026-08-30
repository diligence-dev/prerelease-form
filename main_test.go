package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS events (
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
		t.Fatalf("create events: %v", err)
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
	if err != nil {
		t.Fatalf("create submissions: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustInsertEvent(t *testing.T, db *sql.DB, setCode, date, whatEn, whatDe, whereEn, whereDe, whereLinkEn, whereLinkDe string, draftCap, sealedCap int, draftPrice, sealedPrice float64) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO events (set_code, date, what_en, what_de, where_en, where_de, where_link_en, where_link_de, draft_cap, sealed_cap, draft_price, sealed_price) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		setCode, date, whatEn, whatDe, whereEn, whereDe, whereLinkEn, whereLinkDe, draftCap, sealedCap, draftPrice, sealedPrice)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// mustCreateEvent inserts a standard event "HOB" (caps 24/8, prices 15/30) and
// returns its id plus the set code, so most tests share one fixture.
func mustCreateEvent(t *testing.T, db *sql.DB) (int64, string) {
	t.Helper()
	return mustInsertEvent(t, db, "HOB", "2026-08-07T17:30:00Z", "What EN", "Was DE", "Where EN", "Wo DE", "https://example.com/link", "https://example.com/link", 24, 8, 15, 30), "HOB"
}

func mustInsert(t *testing.T, db *sql.DB, eventID int64, email, name, format, status string) {
	mustInsertLang(t, db, eventID, email, name, format, status, "en")
}

func mustInsertLang(t *testing.T, db *sql.DB, eventID int64, email, name, format, status, lang string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, lang, event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		email, name, format, 0, time.Now().UTC().Format(time.RFC3339), status, lang, eventID)
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
}

func testConfig() config {
	return config{
		weroEmail:         "wero@example.com",
		weroLink:          "https://wero.example.com/pay",
		iban:              "DE1234567890",
		ibanRecipient:     "Test Recipient",
		bic:               "GENODEM1GLS",
		organizerEmail:    "organizer@example.com",
		organizerPassword: "secret-token",
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

// loginAsOrganizer posts the login form and returns the response plus the
// Set-Cookie header value (empty if none). Does not follow redirects.
func loginAsOrganizer(t *testing.T, server *httptest.Server, username, password string) (*http.Response, string) {
	t.Helper()
	client := noRedirectClient()
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	resp, err := client.PostForm(server.URL+"/organizer/login", form)
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	return resp, resp.Header.Get("Set-Cookie")
}

// authedGet performs an authenticated GET on path after logging in.
func authedGet(t *testing.T, server *httptest.Server, path string) *http.Response {
	t.Helper()
	_, setCookie := loginAsOrganizer(t, server, "organizer", "secret-token")
	if setCookie == "" {
		t.Fatalf("login did not set cookie")
	}
	req, _ := http.NewRequest("GET", server.URL+path, nil)
	req.Header.Set("Cookie", setCookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return resp
}

// authedPostForm performs an authenticated POST of form on path after logging in.
func authedPostForm(t *testing.T, server *httptest.Server, path string, form url.Values) *http.Response {
	t.Helper()
	_, setCookie := loginAsOrganizer(t, server, "organizer", "secret-token")
	if setCookie == "" {
		t.Fatalf("login did not set cookie")
	}
	req, _ := http.NewRequest("POST", server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", setCookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// validCreateForm builds a complete, valid event-creation form for setCode.
func validCreateForm(setCode string) url.Values {
	f := url.Values{}
	f.Set("set_code", setCode)
	f.Set("date", "2026-08-07")
	f.Set("time", "17:30")
	f.Set("what_en", "What EN")
	f.Set("what_de", "Was DE")
	f.Set("where_en", "Where EN")
	f.Set("where_de", "Wo DE")
	f.Set("where_link_en", "https://example.com/en")
	f.Set("where_link_de", "https://example.com/de")
	f.Set("draft_cap", "24")
	f.Set("sealed_cap", "8")
	f.Set("draft_price", "15")
	f.Set("sealed_price", "30")
	return f
}

func TestGetIndex(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/HOB/en/")
	if err != nil {
		t.Fatalf("get /HOB/en/: %v", err)
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
	mustCreateEvent(t, db)
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
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/HOB/en/pay") {
		t.Fatalf("redirect = %v, want /HOB/en/pay?email=...", loc)
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
	for _, bad := range []string{"€15", "wero@example.com", "DE1234567890"} {
		if strings.Contains(mailer.sends[0].body, bad) {
			t.Errorf("body contains %q", bad)
		}
	}
}

func TestSubmitValidSealed(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
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
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if strings.Contains(mailer.sends[0].body, "€30") {
		t.Errorf("body should not contain amount: %q", mailer.sends[0].body)
	}
}

func TestSubmitValidationErrors(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
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
			resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestSubmitDuplicateEmail(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
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

	resp, _ := http.PostForm(server.URL+"/HOB/en/submit", form)
	resp.Body.Close()
	client := noRedirectClient()
	resp = postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "already registered") {
		t.Errorf("body missing already registered: %q", string(body))
	}
}

func TestPerEventIsolation(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db) // HOB
	mustInsertEvent(t, db, "LOTR", "2026-09-01T10:00:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "shared@example.com")
	form.Set("name", "Shared")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")

	for _, setCode := range []string{"HOB", "LOTR"} {
		client := noRedirectClient()
		resp := postFormNoRedirect(t, client, server.URL+"/"+setCode+"/en/submit", form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: status = %d, want %d", setCode, resp.StatusCode, http.StatusFound)
		}
		loc, _ := resp.Location()
		if loc == nil || !strings.HasPrefix(loc.Path, "/"+setCode+"/en/pay") {
			t.Fatalf("%s: redirect = %v, want /%s/en/pay?...", setCode, loc, setCode)
		}
	}
}

func TestOrganizerMissingAuthRedirectsToLogin(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	client := noRedirectClient()
	resp, err := client.Get(server.URL + "/organizer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/organizer/login" {
		t.Errorf("redirect = %v, want /organizer/login", loc)
	}
}

func TestOrganizerLoginWrongPassword(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, setCookie := loginAsOrganizer(t, server, "organizer", "wrong")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if setCookie != "" {
		t.Errorf("Set-Cookie should be empty on failed login, got %q", setCookie)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "invalid") {
		t.Errorf("body should contain error message, got: %q", string(body))
	}
}

func TestOrganizerLoginWrongUsername(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, _ := loginAsOrganizer(t, server, "wrong", "secret-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestOrganizerLoginCorrectPasswordSetsCookie(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, setCookie := loginAsOrganizer(t, server, "organizer", "secret-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if setCookie == "" {
		t.Fatalf("Set-Cookie missing on successful login")
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/organizer" {
		t.Errorf("redirect = %v, want /organizer", loc)
	}
	if !strings.Contains(setCookie, "HttpOnly") {
		t.Errorf("cookie not HttpOnly: %q", setCookie)
	}
	if !strings.Contains(setCookie, "organizer_session=") {
		t.Errorf("cookie name missing: %q", setCookie)
	}
}

func TestOrganizerCookieGrantsAccess(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "test@example.com", "Test User", "draft", "confirmed")

	_, setCookie := loginAsOrganizer(t, server, "organizer", "secret-token")
	if setCookie == "" {
		t.Fatalf("login did not set cookie")
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	req, _ := http.NewRequest("GET", server.URL+"/organizer/HOB", nil)
	req.Header.Set("Cookie", setCookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"<table>", "<tr>", "Name", "Email", "Format", "Payment", "Id", "Status"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
	if !strings.Contains(string(body), "Test User") {
		t.Errorf("body missing user name")
	}
}

func TestOrganizerCookieTamperedRejected(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	tampered := "organizer_session=AAAA.BBBB"
	req, _ := http.NewRequest("GET", server.URL+"/organizer", nil)
	req.Header.Set("Cookie", tampered)
	client := noRedirectClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want redirect", resp.StatusCode)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/organizer/login" {
		t.Errorf("redirect = %v, want /organizer/login", loc)
	}
}

func TestOrganizerCookieExpiredRejected(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	cfg := testConfig()
	expired := makeSessionCookie(cfg, time.Now().Add(-1*time.Hour))
	req, _ := http.NewRequest("GET", server.URL+"/organizer", nil)
	req.AddCookie(expired)
	client := noRedirectClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want redirect", resp.StatusCode)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/organizer/login" {
		t.Errorf("redirect = %v, want /organizer/login", loc)
	}
}

func TestOrganizerSortedByStatusThenName(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "cancel@example.com", "Zed Cancelled", "draft", "cancelled")
	mustInsert(t, db, eventID, "wait@example.com", "Alice Waitlist", "draft", "waitlist")
	mustInsert(t, db, eventID, "confirm3@example.com", "Charlie Confirmed", "draft", "confirmed")
	mustInsert(t, db, eventID, "confirm1@example.com", "Alice Confirmed", "draft", "confirmed")
	mustInsert(t, db, eventID, "confirm2@example.com", "Bob Confirmed", "draft", "confirmed")

	resp := authedGet(t, server, "/organizer/HOB")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	aliceIdx := strings.Index(string(body), "Alice Confirmed")
	bobIdx := strings.Index(string(body), "Bob Confirmed")
	charlieIdx := strings.Index(string(body), "Charlie Confirmed")
	aliceWaitIdx := strings.Index(string(body), "Alice Waitlist")
	zedCancelIdx := strings.Index(string(body), "Zed Cancelled")

	if aliceIdx < 0 || bobIdx < 0 || charlieIdx < 0 {
		t.Fatalf("confirmed users not found in output")
	}
	if aliceIdx > bobIdx || bobIdx > charlieIdx {
		t.Errorf("confirmed not sorted A->Z: %v", string(body))
	}
	if aliceWaitIdx < charlieIdx {
		t.Errorf("waitlist should come after confirmed: %v", string(body))
	}
	if zedCancelIdx < aliceWaitIdx {
		t.Errorf("cancelled should come after waitlist: %v", string(body))
	}
}

func TestOrganizerCapacityCounts(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	// Draft: 3 confirmed + 1 waitlist + 1 cancelled (cancelled excluded).
	mustInsert(t, db, eventID, "d1@example.com", "Draft One", "draft", "confirmed")
	mustInsert(t, db, eventID, "d2@example.com", "Draft Two", "draft", "confirmed")
	mustInsert(t, db, eventID, "d3@example.com", "Draft Three", "draft", "confirmed")
	mustInsert(t, db, eventID, "dw@example.com", "Draft Wait", "draft", "waitlist")
	mustInsert(t, db, eventID, "dc@example.com", "Draft Cancel", "draft", "cancelled")
	// Sealed: 2 confirmed + 1 waitlist + 1 cancelled.
	mustInsert(t, db, eventID, "s1@example.com", "Sealed One", "sealed", "confirmed")
	mustInsert(t, db, eventID, "s2@example.com", "Sealed Two", "sealed", "confirmed")
	mustInsert(t, db, eventID, "sw@example.com", "Sealed Wait", "sealed", "waitlist")
	mustInsert(t, db, eventID, "sc@example.com", "Sealed Cancel", "sealed", "cancelled")

	resp := authedGet(t, server, "/organizer/HOB")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// (signed-up + waitlist)/capacity per format: draft 4/24, sealed 3/8.
	if !strings.Contains(string(body), "4/24 draft") {
		t.Errorf("body missing '4/24 draft': %q", string(body))
	}
	if !strings.Contains(string(body), "3/8 sealed") {
		t.Errorf("body missing '3/8 sealed': %q", string(body))
	}
}

func TestOrganizerRowStyling(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "sealed@example.com", "Sealed User", "sealed", "confirmed")
	mustInsert(t, db, eventID, "cancelled@example.com", "Cancelled User", "draft", "cancelled")

	resp := authedGet(t, server, "/organizer/HOB")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "Sealed User") || !strings.Contains(string(body), "class=\"italic\"") {
		t.Errorf("sealed row should have italic class")
	}
	if !strings.Contains(string(body), "Cancelled User") || !strings.Contains(string(body), "class=\"strike\"") {
		t.Errorf("cancelled row should have strike class")
	}
}

func TestOrganizerPerEventCSV(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsertLang(t, db, eventID, "csvtest@example.com", "CSV Test", "draft", "confirmed", "de")

	resp := authedGet(t, server, "/organizer/HOB?export=csv")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv" {
		t.Errorf("content-type = %q, want text/csv", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "attachment; filename=\"HOB.csv\"" {
		t.Errorf("content-disposition = %q, want attachment; filename=\"HOB.csv\"", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	reader := csv.NewReader(bytes.NewReader(body))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("csv read: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("rows = %d, want at least 2", len(rows))
	}
	if rows[0][0] != "id" || rows[0][1] != "email" || rows[0][2] != "name" {
		t.Errorf("header mismatch: %v", rows[0])
	}
	if len(rows[0]) != 10 {
		t.Errorf("header column count = %d, want 10", len(rows[0]))
	}
	if rows[0][9] != "set_code" {
		t.Errorf("10th header = %q, want set_code", rows[0][9])
	}
	if rows[1][9] != "HOB" {
		t.Errorf("set_code value = %q, want HOB", rows[1][9])
	}
	if rows[1][8] != "de" {
		t.Errorf("lang value = %q, want de", rows[1][8])
	}
}

func TestOrganizerCSVExportRequiresAuth(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	client := noRedirectClient()
	resp, err := client.Get(server.URL + "/organizer/HOB?export=csv")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/organizer/login" {
		t.Errorf("redirect = %v, want /organizer/login", loc)
	}
}

func TestHealth(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("get /health: %v", err)
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
	mustCreateEvent(t, db)
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
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/HOB/en/pay") {
		t.Errorf("redirect = %v, want /HOB/en/pay?email=...", loc)
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
	mustCreateEvent(t, db)
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
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/HOB/en/pay") {
		t.Errorf("redirect = %v, want /HOB/en/pay?email=...", loc)
	}
}

func TestDraftWaitlist(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 24; i++ {
		mustInsert(t, db, eventID, fmt.Sprintf("draft%d@example.com", i), fmt.Sprintf("Draft %d", i), "draft", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "waitdraft@example.com")
	form.Set("name", "Wait Draft")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/HOB/en/waitlist" {
		t.Fatalf("redirect = %v, want /HOB/en/waitlist", loc)
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 8; i++ {
		mustInsert(t, db, eventID, fmt.Sprintf("sealed%d@example.com", i), fmt.Sprintf("Sealed %d", i), "sealed", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "waitsealed@example.com")
	form.Set("name", "Wait Sealed")
	form.Set("format", "sealed")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/HOB/en/waitlist" {
		t.Fatalf("redirect = %v, want /HOB/en/waitlist", loc)
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

func TestPerEventCapacities(t *testing.T) {
	db := testDB(t)
	eventID := mustInsertEvent(t, db, "CAP", "2026-08-07T17:30:00Z", "What", "Was", "Where", "Wo", "", "", 3, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 3; i++ {
		mustInsert(t, db, eventID, fmt.Sprintf("draft%d@example.com", i), fmt.Sprintf("Draft %d", i), "draft", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "waitdraft@example.com")
	form.Set("name", "Wait Draft")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/CAP/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/CAP/en/waitlist" {
		t.Fatalf("redirect = %v, want /CAP/en/waitlist", loc)
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
	if !strings.Contains(mailer.sends[0].body, "3 Draft") {
		t.Errorf("body missing '3 Draft': %q", mailer.sends[0].body)
	}
}

func TestSeatCountsAfterSubmissions(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 3; i++ {
		mustInsert(t, db, eventID, fmt.Sprintf("draft%d@example.com", i), fmt.Sprintf("Draft %d", i), "draft", "confirmed")
	}

	resp, err := http.Get(server.URL + "/HOB/en/")
	if err != nil {
		t.Fatalf("get /HOB/en/: %v", err)
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
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/HOB/en/cancel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`<input`, `action="/HOB/en/cancel"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestCancelConfirmedPromotesWaitlist(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "canceler@example.com", "Canceler", "draft", "confirmed")
	mustInsert(t, db, eventID, "waiter@example.com", "Waiter", "draft", "waitlist")

	form := url.Values{}
	form.Set("email", "canceler@example.com")
	resp, err := http.PostForm(server.URL+"/HOB/en/cancel", form)
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
	db.QueryRow("SELECT status FROM submissions WHERE event_id=? AND email=?", eventID, "canceler@example.com").Scan(&cancelStatus)
	db.QueryRow("SELECT status FROM submissions WHERE event_id=? AND email=?", eventID, "waiter@example.com").Scan(&waitStatus)
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
			if !strings.Contains(s.body, "/HOB/en/pay?email=") {
				t.Errorf("promotion email missing /HOB/en/pay?email=: %s", s.body)
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "canceler2@example.com", "Canceler2", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "canceler2@example.com")
	resp, err := http.PostForm(server.URL+"/HOB/en/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var status string
	db.QueryRow("SELECT status FROM submissions WHERE event_id=? AND email=?", eventID, "canceler2@example.com").Scan(&status)
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "waitcancel@example.com", "Wait Cancel", "sealed", "waitlist")

	form := url.Values{}
	form.Set("email", "waitcancel@example.com")
	resp, err := http.PostForm(server.URL+"/HOB/en/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var status string
	db.QueryRow("SELECT status FROM submissions WHERE event_id=? AND email=?", eventID, "waitcancel@example.com").Scan(&status)
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
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "unknown@example.com")
	resp, err := http.PostForm(server.URL+"/HOB/en/cancel", form)
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "already@example.com", "Already", "draft", "cancelled")

	form := url.Values{}
	form.Set("email", "already@example.com")
	resp, err := http.PostForm(server.URL+"/HOB/en/cancel", form)
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

func TestSeatsLeftClampedAtZero(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 30; i++ {
		mustInsert(t, db, eventID, fmt.Sprintf("overdraft%d@example.com", i), fmt.Sprintf("Over %d", i), "draft", "confirmed")
	}

	resp, err := http.Get(server.URL + "/HOB/en/")
	if err != nil {
		t.Fatalf("get /HOB/en/: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "-6") {
		t.Errorf("body contains negative seats: %q", string(body))
	}
}

func TestSubmitRejectsCRLFInName(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
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
		resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want %d", bad, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestSubmitRejectsCRLFInEmail(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
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
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestEPCPayload(t *testing.T) {
	cases := []struct {
		amount float64
		amt    string
	}{
		{15, "EUR15,00"},
		{12.50, "EUR12,50"},
		{25, "EUR25,00"},
	}
	for _, c := range cases {
		got := epcPayload("GENODEM1GLS", "Test Recipient", "DE12345678901234567890", c.amount, "Prerelease id 7")
		want := fmt.Sprintf("BCD\r\n001\r\n1\r\nSCT\r\nGENODEM1GLS\r\nTest Recipient\r\nDE12345678901234567890\r\n%s\r\n\r\n\r\nPrerelease id 7\r\n\r\n", c.amt)
		if got != want {
			t.Errorf("epcPayload(%v) = %q, want %q", c.amount, got, want)
		}
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "draftuser@example.com", "Draft User", "draft", "confirmed")

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=draftuser@example.com")
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "sealeduser@example.com", "Sealed User", "sealed", "confirmed")

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=sealeduser@example.com")
	if err != nil {
		t.Fatalf("get /pay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "€30") {
		t.Errorf("body missing €30: %s", string(body))
	}
}

func TestPerEventPriceRender(t *testing.T) {
	db := testDB(t)
	eventID := mustInsertEvent(t, db, "PRICE", "2026-08-07T17:30:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 20, 40)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "d@example.com", "Draft P", "draft", "confirmed")
	mustInsert(t, db, eventID, "s@example.com", "Sealed P", "sealed", "confirmed")

	resp, err := http.Get(server.URL + "/PRICE/en/pay?email=d@example.com")
	if err != nil {
		t.Fatalf("get draft pay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "€20") {
		t.Errorf("draft pay missing €20: %s", string(body))
	}
	if strings.Contains(string(body), "€15") {
		t.Errorf("draft pay should not show €15: %s", string(body))
	}

	resp, err = http.Get(server.URL + "/PRICE/en/pay?email=s@example.com")
	if err != nil {
		t.Fatalf("get sealed pay: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "€40") {
		t.Errorf("sealed pay missing €40: %s", string(body))
	}
	if strings.Contains(string(body), "€30") {
		t.Errorf("sealed pay should not show €30: %s", string(body))
	}
}

func TestDecimalPriceRender(t *testing.T) {
	db := testDB(t)
	eventID := mustInsertEvent(t, db, "DEC", "2026-08-07T17:30:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 12.50, 25)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "draft@example.com", "Draft", "draft", "confirmed")

	resp, err := http.Get(server.URL + "/DEC/en/")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"€12.50", "€25.00"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing %q: %s", want, string(body))
		}
	}

	resp, err = http.Get(server.URL + "/DEC/en/pay?email=draft@example.com")
	if err != nil {
		t.Fatalf("get pay: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"€12.50", "a=1250"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("pay page missing %q: %s", want, string(body))
		}
	}
}

func TestPayPageUnknownEmail(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=nobody@example.com")
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "waitlist@example.com", "Waitlist User", "draft", "waitlist")

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=waitlist@example.com")
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "cancelled@example.com", "Cancelled User", "draft", "cancelled")

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=cancelled@example.com")
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, payment, event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"paid@example.com", "Paid User", "draft", 0, time.Now().UTC().Format(time.RFC3339), "confirmed", "paid", eventID)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=paid@example.com")
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, payment, event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cash@example.com", "Cash User", "draft", 0, time.Now().UTC().Format(time.RFC3339), "confirmed", "cash", eventID)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := http.Get(server.URL + "/HOB/en/pay?email=cash@example.com")
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
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "weropay@example.com", "Wero Pay", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "weropay@example.com")
	form.Set("method", "wero")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/pay", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if !strings.HasPrefix(loc.Path, "/HOB/en/pay") {
		t.Errorf("redirect = %v, want /HOB/en/pay", loc)
	}

	var payment string
	err := db.QueryRow("SELECT payment FROM submissions WHERE event_id=? AND email=?", eventID, "weropay@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "paid" {
		t.Errorf("payment = %q, want paid", payment)
	}
}

func TestPayMarkPaidIBAN(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "ibanpay@example.com", "IBAN Pay", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "ibanpay@example.com")
	form.Set("method", "iban")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/pay", form)
	defer resp.Body.Close()

	var payment string
	err := db.QueryRow("SELECT payment FROM submissions WHERE event_id=? AND email=?", eventID, "ibanpay@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "paid" {
		t.Errorf("payment = %q, want paid", payment)
	}
}

func TestPayMarkCash(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "cashpay@example.com", "Cash Pay", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "cashpay@example.com")
	form.Set("method", "cash")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/pay", form)
	defer resp.Body.Close()

	var payment string
	err := db.QueryRow("SELECT payment FROM submissions WHERE event_id=? AND email=?", eventID, "cashpay@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "cash" {
		t.Errorf("payment = %q, want cash", payment)
	}
}

func TestPayLockedAfterMark(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	_, err := db.Exec(`INSERT INTO submissions (email, name, format, mailing_list, created_at, status, payment, event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"locked@example.com", "Locked User", "draft", 0, time.Now().UTC().Format(time.RFC3339), "confirmed", "paid", eventID)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	form := url.Values{}
	form.Set("email", "locked@example.com")
	form.Set("method", "cash")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/pay", form)
	defer resp.Body.Close()

	var payment string
	err = db.QueryRow("SELECT payment FROM submissions WHERE event_id=? AND email=?", eventID, "locked@example.com").Scan(&payment)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if payment != "paid" {
		t.Errorf("payment changed to %q, should stay paid", payment)
	}
}

func TestPayInvalidMethod(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "invalid@example.com", "Invalid", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "invalid@example.com")
	form.Set("method", "bogus")
	resp, err := http.PostForm(server.URL+"/HOB/en/pay", form)
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
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "")
	form.Set("method", "wero")
	resp, err := http.PostForm(server.URL+"/HOB/en/pay", form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestParseAcceptLanguage(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"", "en"},
		{"de", "de"},
		{"en", "en"},
		{"de-DE", "de"},
		{"de-DE,de;q=0.9", "de"},
		{"fr-FR,fr;q=0.9,en;q=0.8", "en"},
		{"fr", "en"},
		{"en-US,en;q=0.9", "en"},
		{"de;q=0.5,en;q=0.9", "en"},
		{"de;q=0.5,en;q=0.3", "de"},
		{"es,zh,en;q=0.1", "en"},
		{"xx-YY", "en"},
		{"de;q=0", "en"},
		{"de;q=0, en;q=0", "en"},
		{"de;q=0, en;q=0.8", "en"},
	}
	for _, tc := range cases {
		got := parseAcceptLanguage(tc.header)
		if got != tc.want {
			t.Errorf("parseAcceptLanguage(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestRootRedirectsToLatestEvent(t *testing.T) {
	db := testDB(t)
	mustInsertEvent(t, db, "EARL", "2026-01-01T10:00:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
	mustInsertEvent(t, db, "LATE", "2026-12-01T10:00:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	client := noRedirectClient()
	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/LATE" {
		t.Errorf("redirect = %v, want /LATE (latest by date)", loc)
	}
}

func TestSetCodeRedirectsByAcceptLanguage(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL+"/HOB", nil)
	req.Header.Set("Accept-Language", "de-DE,de;q=0.9")
	client := noRedirectClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get /HOB: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/HOB/de/" {
		t.Errorf("redirect = %v, want /HOB/de/", loc)
	}
}

func TestEventCreation(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	t.Run("auth required", func(t *testing.T) {
		form := validCreateForm("NEW1")
		client := noRedirectClient()
		resp := postFormNoRedirect(t, client, server.URL+"/organizer", form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		loc, _ := resp.Location()
		if loc == nil || loc.Path != "/organizer/login" {
			t.Errorf("redirect = %v, want /organizer/login", loc)
		}
	})

	t.Run("valid", func(t *testing.T) {
		form := validCreateForm("NEW1")
		resp := authedPostForm(t, server, "/organizer", form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		loc, _ := resp.Location()
		if loc == nil || loc.Path != "/organizer/NEW1" {
			t.Errorf("redirect = %v, want /organizer/NEW1", loc)
		}
		var whatEn string
		err := db.QueryRow("SELECT what_en FROM events WHERE set_code=?", "NEW1").Scan(&whatEn)
		if err != nil {
			t.Fatalf("event not inserted: %v", err)
		}
		if whatEn != "What EN" {
			t.Errorf("what_en = %q, want What EN", whatEn)
		}
	})

	t.Run("missing required field", func(t *testing.T) {
		form := validCreateForm("NEW2")
		form.Del("what_en")
		resp := authedPostForm(t, server, "/organizer", form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})

	t.Run("invalid set code", func(t *testing.T) {
		for _, code := range []string{"hob", "A", "ABCDEFGHI", "AB!"} {
			form := validCreateForm(code)
			resp := authedPostForm(t, server, "/organizer", form)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("set_code %q: status = %d, want %d", code, resp.StatusCode, http.StatusBadRequest)
			}
		}
	})

	t.Run("reserved set code", func(t *testing.T) {
		for _, code := range []string{"organizer", "en", "de", "health", "events", "new"} {
			// reserved words are lowercase and thus also fail the regex,
			// but uppercase-reserved variants are covered implicitly; this
			// asserts the reserved list itself rejects them.
			form := validCreateForm(code)
			resp := authedPostForm(t, server, "/organizer", form)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("reserved %q: status = %d, want %d", code, resp.StatusCode, http.StatusBadRequest)
			}
		}
	})

	t.Run("duplicate set code", func(t *testing.T) {
		mustInsertEvent(t, db, "DUP", "2026-08-07T17:30:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
		form := validCreateForm("DUP")
		resp := authedPostForm(t, server, "/organizer", form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "already exists") {
			t.Errorf("body missing 'already exists': %q", string(body))
		}
	})
}

func TestCreateFormPrefill(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp := authedGet(t, server, "/organizer")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		`type="date"`,
		`type="time"`,
		`value="17:30"`,
		`name="where_link_en"`,
		`name="where_link_de"`,
		`Draft & Sealed from English Play Booster Displays (no Prerelease Packs), 3-round-tournament, no prizes`,
		`Draft & Sealed aus englischen Play Booster Displays (keine Prerelease Packs), 3-Runden-Turnier, keine Preise`,
		`Ziegelstr. 4, 10117 Berlin; same Location as weekly draft: `,
		`Ziegelstr. 4, 10117 Berlin; gleicher Ort wie wöchentlicher Draft: `,
		`https://mtg-cube.de/hedwig-english/`,
		`https://mtg-cube.de/hedwig"`,
		`value="12.50"`,
		`value="25"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("create form missing %q", want)
		}
	}
}

func TestCreateEventWithDecimalPrices(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := validCreateForm("DEC")
	form.Set("draft_price", "12.50")
	form.Set("sealed_price", "25")
	resp := authedPostForm(t, server, "/organizer", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	var draftPrice, sealedPrice float64
	err := db.QueryRow("SELECT draft_price, sealed_price FROM events WHERE set_code=?", "DEC").Scan(&draftPrice, &sealedPrice)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if draftPrice != 12.50 {
		t.Errorf("draft_price = %v, want 12.50", draftPrice)
	}
	if sealedPrice != 25 {
		t.Errorf("sealed_price = %v, want 25", sealedPrice)
	}
}

func TestOrganizerEventList(t *testing.T) {
	db := testDB(t)
	mustInsertEvent(t, db, "ALFA", "2026-03-01T10:00:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
	mustInsertEvent(t, db, "BRAVO", "2026-09-01T10:00:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp := authedGet(t, server, "/organizer")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"ALFA", "BRAVO"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing event %q: %q", want, string(body))
		}
	}
	// Newest first: BRAVO (Sep) before ALFA (Mar).
	bravoIdx := strings.Index(string(body), "BRAVO")
	alfaIdx := strings.Index(string(body), "ALFA")
	if bravoIdx < 0 || alfaIdx < 0 || bravoIdx > alfaIdx {
		t.Errorf("events not sorted newest first: %q", string(body))
	}
}

func TestOrganizerEventListDateFormat(t *testing.T) {
	db := testDB(t)
	mustInsertEvent(t, db, "ALFA", "2026-03-01T10:00:00Z", "What", "Was", "Where", "Wo", "", "", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp := authedGet(t, server, "/organizer")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "2026-03-01 10:00") {
		t.Errorf("list date not formatted as yyyy-mm-dd 24h: %s", s)
	}
	if strings.Contains(s, "T10:00:00Z") {
		t.Errorf("list date still raw RFC3339: %s", s)
	}
}

func TestOrganizerEventPage(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "viewer@example.com", "Viewer", "draft", "confirmed")

	t.Run("authed shows submissions and edit form", func(t *testing.T) {
		resp := authedGet(t, server, "/organizer/HOB")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		body, _ := io.ReadAll(resp.Body)
		for _, want := range []string{"Viewer", "Edit event", "Save event"} {
			if !strings.Contains(string(body), want) {
				t.Errorf("body missing %q: %q", want, string(body))
			}
		}
	})

	t.Run("unauthed redirects to login", func(t *testing.T) {
		client := noRedirectClient()
		resp, err := client.Get(server.URL + "/organizer/HOB")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		loc, _ := resp.Location()
		if loc == nil || loc.Path != "/organizer/login" {
			t.Errorf("redirect = %v, want /organizer/login", loc)
		}
	})
}

func TestEditFormSplitWhereLink(t *testing.T) {
	db := testDB(t)
	mustInsertEvent(t, db, "EDIT", "2026-08-07T17:30:00Z", "What", "Was", "Where", "Wo", "https://en.example", "https://de.example", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp := authedGet(t, server, "/organizer/EDIT")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		`name="where_link_en"`,
		`name="where_link_de"`,
		`value="https://en.example"`,
		`value="https://de.example"`,
		`step="0.01"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("edit form missing %q: %s", want, s)
		}
	}
}

func TestOrganizerEventEdit(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := validCreateForm("HOB")
	form.Set("what_en", "Updated What")
	form.Set("draft_cap", "10")
	resp := authedPostForm(t, server, "/organizer/HOB", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/organizer/HOB" {
		t.Errorf("redirect = %v, want /organizer/HOB", loc)
	}

	var whatEn string
	var draftCap int
	err := db.QueryRow("SELECT what_en, draft_cap FROM events WHERE set_code=?", "HOB").Scan(&whatEn, &draftCap)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if whatEn != "Updated What" {
		t.Errorf("what_en = %q, want Updated What", whatEn)
	}
	if draftCap != 10 {
		t.Errorf("draft_cap = %d, want 10", draftCap)
	}
}

func TestUnknownEvent(t *testing.T) {
	db := testDB(t)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/NOPE/en/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Event not found") {
		t.Errorf("body missing 'Event not found': %q", string(body))
	}
}

func TestPerEventWhatWhereRender(t *testing.T) {
	db := testDB(t)
	mustInsertEvent(t, db, "WW", "2026-08-07T17:30:00Z", "English What", "German What", "English Where", "German Where", "https://example.com", "https://example.com", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/WW/en/")
	if err != nil {
		t.Fatalf("get en: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"English What", "English Where"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("en body missing %q: %q", want, string(body))
		}
	}

	resp, err = http.Get(server.URL + "/WW/de/")
	if err != nil {
		t.Fatalf("get de: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"German What", "German Where"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("de body missing %q: %q", want, string(body))
		}
	}
}

func TestWhereLinkPerLang(t *testing.T) {
	db := testDB(t)
	mustInsertEvent(t, db, "WL", "2026-08-07T17:30:00Z", "What", "Was", "Where", "Wo", "https://en.example/link", "https://de.example/link", 24, 8, 15, 30)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/WL/en/")
	if err != nil {
		t.Fatalf("get en: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `href="https://en.example/link"`) {
		t.Errorf("en page missing en where link: %s", string(body))
	}

	resp, err = http.Get(server.URL + "/WL/de/")
	if err != nil {
		t.Fatalf("get de: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `href="https://de.example/link"`) {
		t.Errorf("de page missing de where link: %s", string(body))
	}
}

func TestEmailBodyIncludesSetCode(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "setcode@example.com")
	form.Set("name", "Set Code")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/en/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if !strings.Contains(mailer.sends[0].body, "/HOB/en/pay?email=") {
		t.Errorf("body missing /HOB/en/pay?email=: %q", mailer.sends[0].body)
	}
	if !strings.Contains(mailer.sends[0].body, "/HOB/en/cancel") {
		t.Errorf("body missing /HOB/en/cancel: %q", mailer.sends[0].body)
	}
}

func TestIndexGerman(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	resp, err := http.Get(server.URL + "/HOB/de/")
	if err != nil {
		t.Fatalf("get /HOB/de/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Anmeldung") {
		t.Errorf("body missing German heading: %q", string(body))
	}
	if strings.Contains(string(body), "Sign up") {
		t.Errorf("body should not contain English 'Sign up': %q", string(body))
	}
}

func TestSubmitGermanEmail(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "de@example.com")
	form.Set("name", "Deutsch User")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/de/submit", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || !strings.HasPrefix(loc.Path, "/HOB/de/pay") {
		t.Errorf("redirect = %v, want /HOB/de/pay?...", loc)
	}
	if len(mailer.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(mailer.sends))
	}
	if !strings.Contains(mailer.sends[0].subject, "angemeldet") {
		t.Errorf("subject not German: %q", mailer.sends[0].subject)
	}
	if !strings.Contains(mailer.sends[0].body, "/HOB/de/pay?email=") {
		t.Errorf("body missing /HOB/de/pay link: %q", mailer.sends[0].body)
	}
	if !strings.Contains(mailer.sends[0].body, "/HOB/de/cancel") {
		t.Errorf("body missing /HOB/de/cancel link: %q", mailer.sends[0].body)
	}
}

func TestSubmitLangPersisted(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "delang@example.com")
	form.Set("name", "De Lang")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/de/submit", form)
	resp.Body.Close()

	var lang string
	err := db.QueryRow("SELECT lang FROM submissions WHERE email=?", "delang@example.com").Scan(&lang)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if lang != "de" {
		t.Errorf("lang = %q, want de", lang)
	}
}

func TestPromotionUsesSignupLang(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	for i := 0; i < 24; i++ {
		mustInsert(t, db, eventID, fmt.Sprintf("fill%d@example.com", i), fmt.Sprintf("Fill %d", i), "draft", "confirmed")
	}

	form := url.Values{}
	form.Set("email", "dewaiter@example.com")
	form.Set("name", "De Waiter")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/de/submit", form)
	resp.Body.Close()

	form2 := url.Values{}
	form2.Set("email", "fill0@example.com")
	resp2, err := http.PostForm(server.URL+"/HOB/en/cancel", form2)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp2.StatusCode, http.StatusOK)
	}

	var promotionFound bool
	for _, s := range mailer.sends {
		if s.to == "dewaiter@example.com" && strings.Contains(s.subject, "angemeldet") {
			promotionFound = true
			if !strings.Contains(s.body, "/HOB/de/pay?email=") {
				t.Errorf("promotion body missing /HOB/de/pay link: %q", s.body)
			}
		}
	}
	if !promotionFound {
		t.Errorf("promotion email to waiter not found")
	}
}

func TestCancelReceiptLocalized(t *testing.T) {
	db := testDB(t)
	eventID, _ := mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	mustInsert(t, db, eventID, "delang@example.com", "De Lang", "draft", "confirmed")

	form := url.Values{}
	form.Set("email", "delang@example.com")
	resp, err := http.PostForm(server.URL+"/HOB/de/cancel", form)
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "storniert") {
		t.Errorf("body not German: %q", string(body))
	}
}

func TestInvalidLangRedirects(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	client := noRedirectClient()
	resp, err := client.Get(server.URL + "/HOB/fr/waitlist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, _ := resp.Location()
	if loc == nil || loc.Path != "/HOB/en/waitlist" {
		t.Errorf("redirect = %v, want /HOB/en/waitlist", loc)
	}
}

func TestOrganizerEmailsEnglish(t *testing.T) {
	db := testDB(t)
	mustCreateEvent(t, db)
	mailer := &fakeMailer{fail: map[int]bool{0: true}}
	server := setupTestServer(t, db, mailer)
	defer server.Close()

	form := url.Values{}
	form.Set("email", "defail@example.com")
	form.Set("name", "De Fail")
	form.Set("format", "draft")
	form.Set("cancellation_ack", "on")
	form.Set("data_consent", "on")
	form.Set("mailing_list", "yes")
	client := noRedirectClient()
	resp := postFormNoRedirect(t, client, server.URL+"/HOB/de/submit", form)
	resp.Body.Close()

	if len(mailer.sends) < 2 {
		t.Fatalf("sends = %d, want >= 2", len(mailer.sends))
	}
	notify := mailer.sends[1]
	if notify.to != "organizer@example.com" {
		t.Errorf("notify to = %q, want organizer@example.com", notify.to)
	}
	if !strings.Contains(notify.subject, "Failed to send") {
		t.Errorf("organizer notify subject not English: %q", notify.subject)
	}
}

func TestBuildMessageHeadersAndEncoding(t *testing.T) {
	msg := string(buildMessage("from@example.com", "to@example.com",
		"Prerelease – Du bist angemeldet", "Hallo äöüß, Plaetze frei.\r\n"))

	if !strings.Contains(msg, "Content-Type: text/plain; charset=utf-8\r\n") {
		t.Errorf("missing Content-Type with utf-8 charset:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: 8bit\r\n") {
		t.Errorf("missing Content-Transfer-Encoding: 8bit:\n%s", msg)
	}
	if strings.Contains(msg, "Subject: Prerelease – Du") {
		t.Errorf("subject not RFC 2047 encoded (raw non-ASCII in header):\n%s", msg)
	}
	if !strings.Contains(msg, "Subject: =?utf-8") {
		t.Errorf("subject missing =?utf-8 encoded-word:\n%s", msg)
	}
	if !strings.Contains(msg, "\r\nHallo äöüß, Plaetze frei.\r\n") {
		t.Errorf("body not preserved verbatim:\n%s", msg)
	}
}
