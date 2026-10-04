package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

var ErrYandexBlocked = errors.New("Яндекс требует CAPTCHA или ограничил доступ; автоматизация остановлена")

type BrowserSearch struct {
	Executable, Profile string
	Limit               int
}

// This collector uses the rendered public UI. It deliberately stops at CAPTCHA,
// unexpected markup or failed navigation instead of inferring website absence.
func (b BrowserSearch) Search(ctx context.Context, query string, _ int) ([]Lead, error) {
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	if b.Executable != "" {
		options = append(options, chromedp.ExecPath(b.Executable))
	}
	if b.Profile != "" {
		options = append(options, chromedp.UserDataDir(b.Profile))
	}
	allocator, cancel := chromedp.NewExecAllocator(ctx, options...)
	defer cancel()
	browser, closeBrowser := chromedp.NewContext(allocator)
	defer closeBrowser()
	target := "https://yandex.ru/maps/?text=" + url.QueryEscape(query)
	var raw string
	if e := chromedp.Run(browser, chromedp.Navigate(target), chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Poll(`document.querySelector('a[href*="/org/"]') || document.querySelector('form[action*="checkcaptcha"]') || /captcha|капч|доступ ограничен/i.test(document.body.innerText)`, nil, chromedp.WithPollingTimeout(20*time.Second)), chromedp.Evaluate(collectLinksJS, &raw)); e != nil {
		return nil, errors.New("Не удалось прочитать выдачу Яндекс Карт: проверьте сетевой доступ и доступность браузера")
	}
	var links struct {
		Blocked bool     `json:"blocked"`
		URLs    []string `json:"urls"`
	}
	if json.Unmarshal([]byte(raw), &links) != nil {
		return nil, errors.New("Не удалось разобрать выдачу Яндекса")
	}
	if links.Blocked {
		return nil, ErrYandexBlocked
	}
	if len(links.URLs) == 0 {
		return nil, errors.New("В выдаче нет доступных карточек; отсутствие сайтов не установлено")
	}
	limit := b.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	var leads []Lead
	for _, cardURL := range links.URLs {
		if len(leads) >= limit {
			break
		}
		if !validCardURL(cardURL) {
			continue
		}
		// Serial navigation with a small pause; no CAPTCHA bypass or proxy rotation.
		if e := chromedp.Run(browser, chromedp.Sleep(2*time.Second), chromedp.Navigate(cardURL), chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Poll(`document.querySelector('h1') || document.querySelector('form[action*="checkcaptcha"]')`, nil, chromedp.WithPollingTimeout(15*time.Second)), chromedp.Evaluate(collectCardJS, &raw)); e != nil {
			return nil, errors.New("Чтение карточки не завершилось; результаты текущего запроса не сохранены")
		}
		var card struct {
			Blocked                       bool `json:"blocked"`
			Recognized                    bool `json:"recognized"`
			Name, Address, Website, Phone string
			Socials                       []string `json:"socials"`
		}
		if json.Unmarshal([]byte(raw), &card) != nil {
			return nil, errors.New("Не удалось разобрать карточку")
		}
		if card.Blocked {
			return nil, ErrYandexBlocked
		}
		id := cardID(cardURL)
		if id == "" || card.Name == "" {
			continue
		}
		status := "unknown"
		if card.Recognized {
			status = "not_listed"
			if card.Website != "" {
				status = "listed"
			}
		}
		leads = append(leads, Lead{ID: id, Name: card.Name, Address: card.Address, Website: card.Website, WebsiteStatus: status, MapsURL: cardURL, Phone: card.Phone, Socials: card.Socials, CheckedAt: time.Now().UTC()})
	}
	if len(leads) == 0 {
		return nil, errors.New("Не удалось распознать карточки; требуется обновление браузерного адаптера")
	}
	return leads, nil
}
func validCardURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Hostname() == "yandex.ru" && strings.HasPrefix(u.Path, "/maps/org/") && cardID(raw) != ""
}
func cardID(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "maps" || parts[1] != "org" {
		return ""
	}
	id := parts[len(parts)-1]
	for _, r := range id {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return id
}

const collectLinksJS = `JSON.stringify({blocked:!!document.querySelector('form[action*="checkcaptcha"]')||/captcha|капч|доступ ограничен/i.test(document.body.innerText),urls:[...new Set([...document.querySelectorAll('a[href*="/maps/org/"]')].map(a=>a.href.split('?')[0]))]})`
const collectCardJS = `(()=>{
const text=document.body.innerText;
const blocked=!!document.querySelector('form[action*="checkcaptcha"]')||/captcha|капч|доступ ограничен/i.test(text);
const heading=document.querySelector('h1');
const root=document.querySelector('.business-card-view');
const links=[...document.querySelectorAll('.business-urls-view a[href], a[itemprop="url"]')];
let website='';const socials=[];
for(const a of links){try{const u=new URL(a.href);if(!/^https?:$/.test(u.protocol))continue;if(/(^|\.)(vk\.com|t\.me|telegram\.me|wa\.me|whatsapp\.com)$/.test(u.hostname)){socials.push(a.href)}else if(!/(^|\.)(yandex\.ru|yandex\.net)$/.test(u.hostname)){website=a.href}}catch{}}
const address=document.querySelector('.business-contacts-view__address, [itemprop="address"]');
const phone=document.querySelector('a[href^="tel:"]');
// Website absence is unknown unless the expected contacts panel is rendered.
const recognized=!!root&&!!heading&&!!document.querySelector('.business-contacts-view')&&!document.querySelector('.business-card-view .spinner');
return JSON.stringify({blocked,recognized,name:heading?.innerText.trim()||'',address:address?.innerText.trim()||'',website,phone:phone?.getAttribute('href').replace(/^tel:/,'')||'',socials});
})()`

func statusLabel(s string) string {
	switch s {
	case "listed":
		return "Сайт указан"
	case "not_listed":
		return "Сайт не указан"
	default:
		return "Не удалось определить"
	}
}
func (b BrowserSearch) String() string { return fmt.Sprintf("browser (limit %d)", b.Limit) }
