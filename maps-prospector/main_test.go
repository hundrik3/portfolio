package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestYandexPaginationAndWebsiteStatus(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("apikey") != "test-key" || q.Get("type") != "biz" || q.Get("text") != "Кафе Псков" {
			t.Error("bad API parameters")
		}
		features := []any{}
		if calls == 1 {
			if q.Get("skip") != "0" {
				t.Error("wrong first page")
			}
			for i := 0; i < 50; i++ {
				features = append(features, map[string]any{"properties": map[string]any{"CompanyMetaData": map[string]any{"id": fmt.Sprint(i + 1), "name": "Кафе", "url": "https://example.com"}}})
			}
		} else {
			if q.Get("skip") != "50" {
				t.Error("wrong second page")
			}
			features = append(features, map[string]any{"properties": map[string]any{"CompanyMetaData": map[string]any{"id": "51", "name": "Без ссылки"}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"features": features})
	}))
	defer server.Close()
	g := Yandex{Key: "test-key", Endpoint: server.URL, Client: server.Client()}
	leads, e := g.Search(context.Background(), "Кафе Псков", 3)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || len(leads) != 51 || leads[0].WebsiteStatus != "listed" || leads[50].WebsiteStatus != "not_listed" {
		t.Fatalf("bad results: calls=%d count=%d", calls, len(leads))
	}
}
func TestYandexErrorsDoNotLeakKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret-key-value", 403) }))
	defer server.Close()
	g := Yandex{Key: "secret-key-value", Endpoint: server.URL, Client: server.Client()}
	_, e := g.Search(context.Background(), "Кафе", 1)
	if e == nil || strings.Contains(e.Error(), "secret-key-value") {
		t.Fatal("error missing or leaked key")
	}
	g.Key = ""
	_, e = g.Search(context.Background(), "Кафе", 1)
	if e == nil {
		t.Fatal("missing key accepted")
	}
}
func TestStoreDeduplicatesPersistsAndPreservesSelection(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "leads.json")}
	if e := s.update([]Lead{{ID: "1", Name: "Before", WebsiteStatus: "not_listed"}}, ""); e != nil {
		t.Fatal(e)
	}
	if e := s.update(nil, "1"); e != nil {
		t.Fatal(e)
	}
	if e := s.update([]Lead{{ID: "1", Name: "After", WebsiteStatus: "listed", Website: "https://example.com"}}, ""); e != nil {
		t.Fatal(e)
	}
	other := &Store{Path: s.Path}
	if e := other.load(); e != nil {
		t.Fatal(e)
	}
	if len(other.Leads) != 1 || !other.Leads[0].Selected || other.Leads[0].Name != "After" {
		t.Fatal("lost updates")
	}
}
func TestHTTPFormsEscapingAndExport(t *testing.T) {
	d := t.TempDir()
	s := &Store{Path: filepath.Join(d, "leads.json"), Leads: []Lead{{ID: "1", Name: "<script>alert(1)</script>", Selected: true, WebsiteStatus: "not_listed"}, {ID: "2", Selected: true, WebsiteStatus: "unknown"}, {ID: "3", Selected: true, WebsiteStatus: "listed", Website: "https://example.com"}}}
	a := &App{Store: s, Queue: &Queue{Path: filepath.Join(d, "queue.json")}, CSRF: "test"}
	h := a.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "<script>alert(1)</script>") || !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Fatal("page escaping failed")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/select", strings.NewReader("id=1")))
	if rec.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/export", nil))
	var drafts []map[string]any
	if e := json.Unmarshal(rec.Body.Bytes(), &drafts); e != nil {
		t.Fatal(e)
	}
	if len(drafts) != 1 {
		t.Fatal("export includes noncandidates")
	}
	req := httptest.NewRequest("POST", "/select", strings.NewReader(url.Values{"csrf": {"test"}, "id": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 303 || s.snapshot()[0].Selected {
		t.Fatal("selection failed")
	}
}

type fakeSender struct {
	Calls int
	Err   error
}

func (s *fakeSender) Send(context.Context, Job) (string, error) {
	s.Calls++
	return "provider-1", s.Err
}
func testQueue(t *testing.T) (*Queue, []Lead) {
	t.Helper()
	q := &Queue{Path: filepath.Join(t.TempDir(), "queue.json")}
	leads := []Lead{{ID: "1", WebsiteStatus: "not_listed"}, {ID: "2", WebsiteStatus: "unknown"}, {ID: "3", WebsiteStatus: "listed"}}
	for _, l := range leads {
		if e := q.addContact(Contact{LeadID: l.ID, Channel: "vk", Recipient: "-123"}); e != nil {
			t.Fatal(e)
		}
	}
	if e := q.enqueue(leads); e != nil {
		t.Fatal(e)
	}
	return q, leads
}
func TestQueueSingleAttemptDedupAndEligibility(t *testing.T) {
	q, leads := testQueue(t)
	if e := q.enqueue(leads); e != nil {
		t.Fatal(e)
	}
	if len(q.snapshot().Jobs) != 1 {
		t.Fatal("duplicates or ineligible jobs")
	}
	s := &fakeSender{}
	for i := 0; i < 2; i++ {
		if e := q.tick(context.Background(), s, leads, 10); e != nil {
			t.Fatal(e)
		}
	}
	if s.Calls != 1 || q.snapshot().Jobs[0].Status != "sent" {
		t.Fatal("duplicate send")
	}
	restored := &Queue{Path: q.Path}
	if e := restored.load(); e != nil {
		t.Fatal(e)
	}
	if restored.snapshot().Jobs[0].ProviderID != "provider-1" {
		t.Fatal("lost send result")
	}
}
func TestQueueTimeoutNotRetriedAndCrashRecovered(t *testing.T) {
	q, leads := testQueue(t)
	s := &fakeSender{Err: errors.New("timeout")}
	for i := 0; i < 2; i++ {
		if e := q.tick(context.Background(), s, leads, 10); e != nil {
			t.Fatal(e)
		}
	}
	if s.Calls != 1 || q.snapshot().Jobs[0].Status != "uncertain" {
		t.Fatal("unsafe retry")
	}
	q.State.Jobs[0].Status = "sending"
	if e := q.save(); e != nil {
		t.Fatal(e)
	}
	r := &Queue{Path: q.Path}
	if e := r.load(); e != nil {
		t.Fatal(e)
	}
	if r.snapshot().Jobs[0].Status != "uncertain" {
		t.Fatal("crash led to retry")
	}
}
func TestQueueDailyLimitAndNewWebsite(t *testing.T) {
	q, leads := testQueue(t)
	q.State.Jobs = append(q.State.Jobs, Job{ID: "old", Status: "sent", AttemptedAt: time.Now().UTC()})
	s := &fakeSender{}
	if e := q.tick(context.Background(), s, leads, 1); e != nil {
		t.Fatal(e)
	}
	if s.Calls != 0 {
		t.Fatal("daily limit ignored")
	}
	leads[0].WebsiteStatus = "listed"
	if e := q.tick(context.Background(), s, leads, 10); e != nil {
		t.Fatal(e)
	}
	if s.Calls != 0 {
		t.Fatal("sent after website appeared")
	}
}
func TestBrowserCardURLValidation(t *testing.T) {
	for _, raw := range []string{"https://evil.example/maps/org/name/123/", "https://yandex.ru/other/123/", "https://yandex.ru/maps/org/name/not-id/", "http://yandex.ru/maps/org/name/123/"} {
		if validCardURL(raw) {
			t.Errorf("accepted %s", raw)
		}
	}
	if !validCardURL("https://yandex.ru/maps/org/name/123/") {
		t.Fatal("valid URL rejected")
	}
}
func TestBrowserRenderedFixtures(t *testing.T) {
	if os.Getenv("TEST_BROWSER") != "1" {
		t.Skip("set TEST_BROWSER=1 to execute actual Chromium fixture tests")
	}
	a, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath("/usr/bin/chromium"), chromedp.NoSandbox, chromedp.Env("XDG_CONFIG_HOME="+t.TempDir()))...)
	defer cancel()
	ctx, closeBrowser := chromedp.NewContext(a)
	defer closeBrowser()
	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()
	cases := []struct {
		HTML                string
		Recognized, Blocked bool
		Site                string
	}{
		{`<div class="business-card-view"><h1>Кафе</h1><div class="business-contacts-view">Адрес</div><div class="business-urls-view"><a href="https://example.com">Сайт</a><a href="https://vk.com/cafe">VK</a></div></div>`, true, false, "https://example.com/"},
		{`<h1>Неполная загрузка</h1>`, false, false, ""},
		{`<form action="/checkcaptcha">Введите CAPTCHA</form>`, false, true, ""},
	}
	for _, tt := range cases {
		var raw string
		encoded, _ := json.Marshal(tt.HTML)
		e := chromedp.Run(ctx, chromedp.Evaluate("document.body.innerHTML = "+string(encoded), nil), chromedp.Evaluate(collectCardJS, &raw))
		if e != nil {
			t.Fatal(e)
		}
		var card struct {
			Recognized bool
			Blocked    bool
			Website    string
		}
		if e = json.Unmarshal([]byte(raw), &card); e != nil {
			t.Fatal(e)
		}
		if card.Recognized != tt.Recognized || card.Blocked != tt.Blocked || card.Website != tt.Site {
			t.Fatalf("bad parsed card: %+v", card)
		}
	}
}

func TestCampaignFiltersPopulation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cities.csv")
	os.WriteFile(path, []byte("city,population,population_year,source\nМалый,100000,2025,test-fixture\nБольшой,500000,2025,test-fixture\nМалый,100000,2025,test-fixture\n"), 0600)
	cities, e := loadCities(path)
	if e != nil {
		t.Fatal(e)
	}
	if len(cities) != 1 || cities[0].Name != "Малый" {
		t.Fatal("city filter or dedup failed")
	}
	queries := campaignQueries(cities, "кафе,автосервисы")
	if len(queries) != 2 || queries[0] != "кафе в городе Малый" {
		t.Fatal("wrong campaign queries")
	}
}

func TestPublishedContactsAndRecipientDedup(t *testing.T) {
	c, ok := publishedContact(Lead{ID: "1", Socials: []string{"https://wa.me/79991234567"}})
	if !ok || c.Channel != "whatsapp" || c.Recipient != "79991234567" {
		t.Fatal("WhatsApp link not parsed")
	}
	c, ok = publishedContact(Lead{ID: "1", Socials: []string{"https://t.me/business_contact"}})
	if !ok || c.Channel != "telegram" || c.Recipient != "business_contact" {
		t.Fatal("Telegram link not parsed")
	}
	_, ok = publishedContact(Lead{ID: "1", Socials: []string{"https://t.me/+invite"}, Phone: "не указан"})
	if ok {
		t.Fatal("guessed unsupported contact")
	}
	if normalizePhone("8 (999) 123-45-67") != "79991234567" {
		t.Fatal("phone normalization failed")
	}
	q := &Queue{Path: filepath.Join(t.TempDir(), "queue.json")}
	leads := []Lead{{ID: "1", WebsiteStatus: "not_listed", Phone: "+79991234567"}, {ID: "2", WebsiteStatus: "not_listed", Phone: "+79991234567"}}
	if e := q.discover(leads); e != nil {
		t.Fatal(e)
	}
	if e := q.enqueue(leads); e != nil {
		t.Fatal(e)
	}
	if len(q.snapshot().Jobs) != 1 {
		t.Fatal("duplicate messages to shared recipient")
	}
}
func TestPersonalCredentialsAndLoginFailClosed(t *testing.T) {
	t.Setenv("TELEGRAM_API_ID", "")
	t.Setenv("TELEGRAM_API_HASH", "")
	s := PersonalSender{Directory: t.TempDir()}
	_, e := s.Send(context.Background(), Job{Contact: Contact{Channel: "telegram", Recipient: "business_contact"}})
	if e == nil {
		t.Fatal("Telegram without credentials accepted")
	}
	_, e = s.Send(context.Background(), Job{Contact: Contact{Channel: "whatsapp", Recipient: "79991234567"}})
	if e == nil {
		t.Fatal("WhatsApp without pairing accepted")
	}
}

type unavailableSender struct{ fakeSender }

func (s *unavailableSender) Ready(Contact) error { return errors.New("account not connected") }
func TestQueueWaitsForConnectionWithoutAttempt(t *testing.T) {
	q, leads := testQueue(t)
	s := &unavailableSender{}
	if e := q.tick(context.Background(), s, leads, 10); e != nil {
		t.Fatal(e)
	}
	job := q.snapshot().Jobs[0]
	if s.Calls != 0 || job.Status != "pending" || !job.AttemptedAt.IsZero() || job.Error == "" {
		t.Fatal("missing connection consumed an attempt")
	}
}

func TestPopulationImportAndNoOverwriteOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cities.csv")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":{"bindings":[{"cityLabel":{"value":"Проверочный город"},"city":{"value":"https://www.wikidata.org/entity/Q123"},"population":{"value":"100000"},"date":{"value":"2025-01-01T00:00:00Z"}},{"cityLabel":{"value":"Большой"},"city":{"value":"https://www.wikidata.org/entity/Q456"},"population":{"value":"500000"},"date":{"value":"2025-01-01T00:00:00Z"}}]}}`)
	}))
	defer server.Close()
	if e := refreshCities(context.Background(), path, server.URL, server.Client()); e != nil {
		t.Fatal(e)
	}
	cities, e := loadCities(path)
	if e != nil || len(cities) != 1 {
		t.Fatal("population import failed", e)
	}
	before, _ := os.ReadFile(path)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "blocked", 403) }))
	defer bad.Close()
	if e = refreshCities(context.Background(), path, bad.URL, bad.Client()); e == nil {
		t.Fatal("failed source accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("overwrote existing cities on failure")
	}
}
