package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const outreach = "Здравствуйте! Я веб-разработчик, занимаюсь созданием сайтов. Могу бесплатно сделать для вас демонстрационный вариант сайта. Посмотрите: если понравится, обсудим дальнейшую работу."

type Lead struct {
	WebsiteStatus string    `json:"website_status"`
	Socials       []string  `json:"socials,omitempty"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Address       string    `json:"address"`
	Website       string    `json:"website"`
	MapsURL       string    `json:"maps_url"`
	Phone         string    `json:"phone"`
	CheckedAt     time.Time `json:"checked_at"`
	Selected      bool      `json:"selected"`
}
type Yandex struct {
	Key, Endpoint string
	Client        *http.Client
}

func (g Yandex) Search(ctx context.Context, query string, maxPages int) ([]Lead, error) {
	if g.Key == "" {
		return nil, errors.New("Не задан YANDEX_MAPS_API_KEY. Нужен ключ API поиска организаций Яндекса; ключ JavaScript API не подходит.")
	}
	var leads []Lead
	for page := 0; page < maxPages; page++ {
		u, err := url.Parse(g.Endpoint)
		if err != nil {
			return nil, errors.New("Некорректный адрес API Яндекса")
		}
		q := u.Query()
		q.Set("apikey", g.Key)
		q.Set("text", query)
		q.Set("type", "biz")
		q.Set("lang", "ru_RU")
		q.Set("results", "50")
		q.Set("skip", strconv.Itoa(page*50))
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, errors.New("Не удалось создать запрос Яндекса")
		}
		res, err := g.Client.Do(req)
		if err != nil {
			return nil, errors.New("Не удалось обратиться к Яндексу; проверьте соединение")
		}
		payload, readErr := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if readErr != nil {
			return nil, errors.New("Не удалось прочитать ответ Яндекса")
		}
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("Яндекс: HTTP %d; проверьте ключ API поиска организаций, тариф, квоту и сетевой доступ", res.StatusCode)
		}
		var result struct {
			Features []struct {
				Properties struct {
					Company struct {
						ID      string `json:"id"`
						Name    string `json:"name"`
						Address string `json:"address"`
						URL     string `json:"url"`
						Phones  []struct {
							Formatted string `json:"formatted"`
						} `json:"Phones"`
					} `json:"CompanyMetaData"`
				} `json:"properties"`
			} `json:"features"`
		}
		if err := json.Unmarshal(payload, &result); err != nil {
			return nil, errors.New("Некорректный ответ API Яндекса")
		}
		for _, f := range result.Features {
			p := f.Properties.Company
			if p.ID == "" {
				continue
			}
			phone := ""
			if len(p.Phones) > 0 {
				phone = p.Phones[0].Formatted
			}
			leads = append(leads, Lead{ID: p.ID, Name: p.Name, Address: p.Address, Website: p.URL, WebsiteStatus: websiteStatus(p.URL), MapsURL: "https://yandex.ru/maps/?ol=biz&oid=" + url.QueryEscape(p.ID), Phone: phone, CheckedAt: time.Now().UTC()})
		}
		if len(result.Features) < 50 {
			break
		}
	}
	return leads, nil
}

type Store struct {
	mu    sync.Mutex
	Path  string
	Leads []Lead
}

func (s *Store) load() error {
	b, e := os.ReadFile(s.Path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	return json.Unmarshal(b, &s.Leads)
}
func (s *Store) snapshot() []Lead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Lead(nil), s.Leads...)
}
func (s *Store) update(in []Lead, selection string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]Lead(nil), s.Leads...)
	for _, lead := range in {
		found := false
		for i := range next {
			if next[i].ID == lead.ID {
				lead.Selected = next[i].Selected
				next[i] = lead
				found = true
				break
			}
		}
		if !found {
			next = append(next, lead)
		}
	}
	if selection != "" {
		for i := range next {
			if next[i].ID == selection {
				next[i].Selected = !next[i].Selected
			}
		}
	}
	b, e := json.MarshalIndent(next, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(s.Path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(s.Path), ".leads-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), s.Path); e != nil {
		return e
	}
	s.Leads = next
	return nil
}

type Searcher interface {
	Search(context.Context, string, int) ([]Lead, error)
}

func websiteStatus(site string) string {
	if site == "" {
		return "not_listed"
	}
	return "listed"
}

type App struct {
	Searcher Searcher
	Queue    *Queue
	Sending  bool
	Store    *Store
	Yandex   Yandex
	CSRF     string
	SearchMu sync.Mutex
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		page.Execute(w, struct {
			Leads          []Lead
			CSRF, Template string
			Sending        bool
		}{a.Store.snapshot(), a.CSRF, outreach, a.Sending})
	})
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		if !a.validForm(w, r) {
			return
		}
		query := strings.TrimSpace(r.FormValue("query"))
		if query == "" || len(query) > 500 {
			http.Error(w, "Укажите город и категорию (до 500 байт)", 400)
			return
		}
		if !a.SearchMu.TryLock() {
			http.Error(w, "Поиск уже выполняется", 409)
			return
		}
		defer a.SearchMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		leads, err := a.Searcher.Search(ctx, query, 3)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		if err = a.Store.update(leads, ""); err != nil {
			http.Error(w, "Не удалось сохранить результаты", 500)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /select", func(w http.ResponseWriter, r *http.Request) {
		if !a.validForm(w, r) {
			return
		}
		if err := a.Store.update(nil, r.FormValue("id")); err != nil {
			http.Error(w, "Не удалось сохранить выбор", 500)
			return
		}
		http.Redirect(w, r, "/", 303)
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(a.Queue.snapshot())
	})
	mux.HandleFunc("POST /contact", func(w http.ResponseWriter, r *http.Request) {
		if !a.validForm(w, r) {
			return
		}
		id := r.FormValue("id")
		found := false
		for _, l := range a.Store.snapshot() {
			if l.ID == id {
				found = true
			}
		}
		if !found {
			http.Error(w, "Неизвестный бизнес", 400)
			return
		}
		if e := a.Queue.addContact(Contact{LeadID: id, Channel: r.FormValue("channel"), Recipient: strings.TrimSpace(r.FormValue("recipient"))}); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		if e := a.Queue.enqueue(a.Store.snapshot()); e != nil {
			http.Error(w, "Не удалось создать очередь", 500)
			return
		}
		http.Redirect(w, r, "/", 303)
	})
	mux.HandleFunc("GET /export", func(w http.ResponseWriter, r *http.Request) {
		type Draft struct {
			Lead    Lead   `json:"lead"`
			Message string `json:"message"`
			Status  string `json:"status"`
		}
		drafts := []Draft{}
		for _, l := range a.Store.snapshot() {
			if l.Selected && l.WebsiteStatus == "not_listed" {
				drafts = append(drafts, Draft{l, outreach, "draft_not_sent"})
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="outreach-drafts.json"`)
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(drafts)
	})
	return mux
}
func (a *App) validForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("csrf") != a.CSRF {
		http.Error(w, "Некорректный запрос", 403)
		return false
	}
	return true
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{"statusLabel": statusLabel}).Parse(`<!doctype html><html lang="ru"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Поиск бизнеса без сайта</title><style>body{font:16px system-ui;max-width:1100px;margin:40px auto;padding:20px;background:#f7f8fc;color:#172033}input,button{padding:10px}table{width:100%;border-collapse:collapse;background:white}td,th{padding:12px;border-bottom:1px solid #ddd;text-align:left}small{color:#536078}.box{background:white;padding:20px;margin:20px 0;border-radius:12px}a{color:#2355c7}</style><h1>Поиск бизнеса без сайта</h1><p>Яндекс Карты → проверка ссылки на сайт → кандидаты → черновики обращений</p><div class="box"><p>Поиск работает через браузер без ключа. API-режим доступен при наличии ключа API поиска организаций Яндекса.</p><form action="/search" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>Категория и город <input name="query" placeholder="Салоны красоты в Пскове" required maxlength="250"></label> <button>Найти (до 20 карточек в браузерном режиме)</button></form><small>Браузерный режим не требует API-ключа; API-режим оплачивается по тарифам Яндекса. Результат ограничен доступной выдачей и не является полным перечнем бизнесов города.</small></div><div class="box"><h2>Шаблон обращения</h2><p><strong>Автоотправка: {{if .Sending}}включена{{else}}выключена{{end}}</strong></p><p>{{.Template}}</p><p>Экспорт содержит черновики. Автоотправка включается флагом --send после подключения аккаунтов. Telegram и WhatsApp подключаются командой локального входа; VK подключается локальным входом в Chrome; дополнительный API-режим требует пользовательский токен с доступом к сообщениям. Укажите проверенный контакт бизнеса. Опубликованные номера и ссылки Telegram/WhatsApp/VK добавляются автоматически; имена аккаунтов не угадываются. VK отправляет от личного профиля в сообщества с доступными сообщениями.</p><a href="/export">Скачать черновики выбранных кандидатов (JSON)</a></div><p>Отсутствие ссылки в карточке не доказывает отсутствие сайта. Проверяйте кандидатов перед обращением.</p><p><a href="/queue">Статусы очереди (JSON)</a></p><table><tr><th>Бизнес</th><th>Сайт в карточке</th><th>Контакт</th><th>Выбор</th></tr>{{range .Leads}}<tr><td><strong>{{.Name}}</strong><br>{{.Address}}<br><a href="{{.MapsURL}}" target="_blank" rel="noreferrer">Карточка Яндекс Карты</a><br><small>Проверено {{.CheckedAt.Format "2006-01-02 15:04 UTC"}}</small></td><td>{{if .Website}}<a href="{{.Website}}" rel="noreferrer" target="_blank">Сайт указан</a>{{else}}{{statusLabel .WebsiteStatus}}{{end}}</td><td>{{.Phone}}{{range .Socials}}<br><a href="{{.}}" target="_blank" rel="noreferrer">Соцсеть</a>{{end}}<form method="post" action="/contact"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><select name="channel"><option value="telegram">Telegram</option><option value="whatsapp">WhatsApp</option><option value="vk">VK</option></select><input name="recipient" placeholder="TG username / WA номер / VK ссылка" required><button>Добавить в очередь</button></form></td><td>{{if eq .WebsiteStatus "not_listed"}}<form action="/select" method="post"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><button>{{if .Selected}}Убрать{{else}}Выбрать{{end}}</button></form>{{else}}—{{end}}</td></tr>{{else}}<tr><td colspan="4">Поиск ещё не выполнялся. Введите категорию и город.</td></tr>{{end}}</table></html>`))

func main() {
	addr := flag.String("listen", "127.0.0.1:8080", "HTTP address; use loopback unless protected by an authenticated reverse proxy")
	path := flag.String("data", "data/leads.json", "persistent JSON store")
	mode := flag.String("source", "browser", "browser or api")
	chromium := flag.String("chromium", "", "Chrome/Chromium executable; automatically detected when empty")
	enableSend := flag.Bool("send", false, "Enable automatic dispatch to supplied recipients")
	daily := flag.Int("daily-limit", 10, "Maximum attempts per UTC day")
	interval := flag.Duration("send-interval", 5*time.Minute, "Minimum time between message attempts")
	query := flag.String("query", "", "Optional repeated search query, including city and category")
	scanInterval := flag.Duration("scan-interval", 24*time.Hour, "Repeated search interval")
	citiesFile := flag.String("cities", "", "CSV with city,population,population_year,source; only population <500000")
	categories := flag.String("categories", defaultCategories, "Comma-separated search categories")
	login := flag.String("login", "", "Connect personal account: telegram or whatsapp, locally in a TTY")
	refresh := flag.String("refresh-cities", "", "Fetch candidate Russian cities from Wikidata into the specified CSV, then exit")
	checkVK := flag.Bool("check-vk", false, "Read-only verification of personal VK account and messages permission")
	vkBackend := flag.String("vk-backend", "browser", "VK personal-account transport: browser or api")
	testVKGroup := flag.String("test-vk-group", "", "Send one connection-test message to your own test community URL")
	flag.Parse()
	if *vkBackend != "browser" && *vkBackend != "api" {
		log.Fatal("vk-backend must be browser or api")
	}
	if *testVKGroup != "" {
		recipient, e := vkRecipient(*testVKGroup)
		if e != nil {
			log.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		sender := PersonalSender{Directory: filepath.Dir(*path), VKBackend: *vkBackend, VKBrowser: VKBrowser{Directory: filepath.Dir(*path), Executable: *chromium}, VK: Channels{Client: &http.Client{Timeout: 20 * time.Second}, VKToken: os.Getenv("VK_ACCESS_TOKEN")}}
		contact := Contact{LeadID: "self-test", Channel: "vk", Recipient: recipient}
		if e = sender.Ready(contact); e != nil {
			log.Fatal(e)
		}
		id, e := sender.Send(ctx, Job{ID: fmt.Sprintf("self-test:%s:%d", recipient, time.Now().UnixNano()), Contact: contact, Message: "Тест подключения Maps Prospector. Проверка собственного сообщества."})
		if e != nil {
			log.Fatal(e)
		}
		log.Printf("Test message confirmed: %s", id)
		return
	}

	if *checkVK {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vk := Channels{Client: &http.Client{Timeout: 20 * time.Second}, VKToken: os.Getenv("VK_ACCESS_TOKEN")}
		if _, e := vk.checkVKPersonal(ctx); e != nil {
			log.Fatal(e)
		}
		log.Print("Personal VK token and messages permission confirmed; no message sent")
		return
	}
	if *refresh != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if e := refreshCities(ctx, *refresh, "https://query.wikidata.org/sparql", &http.Client{Timeout: 90 * time.Second}); e != nil {
			log.Fatal(e)
		}
		log.Print("City candidates saved; review population sources before production")
		return
	}
	if *login != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var e error
		if *login == "vk" {
			e = (VKBrowser{Directory: filepath.Dir(*path), Executable: *chromium}).Login(ctx)
		} else {
			e = loginPersonal(ctx, filepath.Dir(*path), *login)
		}
		if e != nil {
			log.Fatal(e)
		}
		return
	}
	queries := []string{}
	if *query != "" {
		queries = append(queries, *query)
	}
	if *citiesFile != "" {
		cities, e := loadCities(*citiesFile)
		if e != nil {
			log.Fatal(e)
		}
		queries = append(queries, campaignQueries(cities, *categories)...)
	}
	if *daily < 1 || *daily > 100 || *interval < time.Minute || *scanInterval < time.Hour {
		log.Fatal("Invalid limits: daily 1..100, send interval >=1m, scan interval >=1h")
	}
	if *mode != "browser" && *mode != "api" {
		log.Fatal("source must be browser or api")
	}
	queue := &Queue{Path: filepath.Join(filepath.Dir(*path), "queue.json")}
	if e := queue.load(); e != nil {
		log.Fatal(e)
	}
	store := &Store{Path: *path}
	if err := store.load(); err != nil {
		log.Fatal("Cannot read lead store: ", err)
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		log.Fatal(err)
	}
	app := &App{Store: store, CSRF: hex.EncodeToString(token), Yandex: Yandex{Key: os.Getenv("YANDEX_MAPS_API_KEY"), Endpoint: "https://search-maps.yandex.ru/v1/", Client: &http.Client{Timeout: 20 * time.Second}}}
	app.Queue = queue
	app.Sending = *enableSend
	app.Searcher = app.Yandex
	if *mode == "browser" {
		app.Searcher = BrowserSearch{Executable: *chromium, Profile: filepath.Join(filepath.Dir(*path), "browser-profile"), Limit: 20}
	}
	if *enableSend {
		sender := PersonalSender{Directory: filepath.Dir(*path), VKBackend: *vkBackend, VKBrowser: VKBrowser{Directory: filepath.Dir(*path), Executable: *chromium}, VK: Channels{Client: &http.Client{Timeout: 20 * time.Second}, VKToken: os.Getenv("VK_ACCESS_TOKEN")}}
		go func() {
			ticker := time.NewTicker(*interval)
			defer ticker.Stop()
			for range ticker.C {
				if e := queue.discover(store.snapshot()); e != nil {
					log.Printf("Contact discovery error: %v", e)
					continue
				}
				if e := queue.enqueue(store.snapshot()); e != nil {
					log.Printf("Queue error: %v", e)
					continue
				}
				if e := queue.tick(context.Background(), sender, store.snapshot(), *daily); e != nil {
					log.Printf("Dispatch persistence error: %v", e)
				}
			}
		}()
	}
	if len(queries) > 0 {
		go func() {
			idx := 0
			for {
				if app.SearchMu.TryLock() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					leads, e := app.Searcher.Search(ctx, queries[idx], 3)
					cancel()
					app.SearchMu.Unlock()
					if e != nil {
						log.Printf("Scheduled search: %v", e)
						if errors.Is(e, ErrYandexBlocked) {
							return
						}
					} else if e = store.update(leads, ""); e != nil {
						log.Printf("Scheduled save: %v", e)
					}
					idx = (idx + 1) % len(queries)
				}
				time.Sleep(*scanInterval)
			}
		}()
	}
	server := &http.Server{Addr: *addr, Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 6 * time.Minute, WriteTimeout: 6 * time.Minute, IdleTimeout: 5 * time.Minute}
	log.Printf("Listening on %s; automatic dispatch enabled=%t", *addr, *enableSend)
	log.Fatal(server.ListenAndServe())
}
