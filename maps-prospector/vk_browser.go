package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

type VKBrowser struct{ Directory, Executable string }

func (b VKBrowser) profile() string { return filepath.Join(b.Directory, "accounts", "vk-browser") }
func (b VKBrowser) Ready() error {
	if _, e := os.Stat(filepath.Join(b.Directory, "accounts", "vk-browser-ready")); e != nil {
		return errors.New("VK: выполните локальный вход --login vk через окно Chrome")
	}
	if _, e := os.Stat(filepath.Join(b.Directory, "accounts", "vk-browser-paused")); e == nil {
		return errors.New("VK: браузерная отправка приостановлена; проверьте диалог и снова выполните --login vk")
	}
	return nil
}
func (b VKBrowser) open(ctx context.Context, visible bool) (context.Context, func()) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.UserDataDir(b.profile()), chromedp.Flag("headless", !visible))
	if b.Executable != "" {
		opts = append(opts, chromedp.ExecPath(b.Executable))
	}
	a, cancel := chromedp.NewExecAllocator(ctx, opts...)
	browser, closeBrowser := chromedp.NewContext(a)
	return browser, func() { closeBrowser(); cancel() }
}
func (b VKBrowser) Login(ctx context.Context) error {
	if _, e := sessionDirectory(b.Directory); e != nil {
		return e
	}
	browser, closeBrowser := b.open(ctx, true)
	defer closeBrowser()
	if e := chromedp.Run(browser, chromedp.Navigate("https://vk.com/"), chromedp.WaitReady("body", chromedp.ByQuery)); e != nil {
		return errors.New("Не удалось открыть Chrome и VK; проверьте установленный Chrome и сеть")
	}
	fmt.Fprintln(os.Stdout, "Войдите в свой личный VK в открывшемся Chrome. Пароли и коды вводите только на сайте VK. Затем вернитесь в этот терминал и нажмите Enter.")
	if _, e := bufio.NewReader(os.Stdin).ReadString('\n'); e != nil {
		return errors.New("Локальный вход прерван")
	}
	var logged bool
	if e := chromedp.Run(browser, chromedp.Evaluate(`!![...document.querySelectorAll('a[href]')].find(a=>a.getClientRects().length && (/^\/(im|messenger)(\?|\/|$)/.test(new URL(a.href).pathname+new URL(a.href).search)))`, &logged)); e != nil || !logged {
		return errors.New("Вход не подтверждён: откройте раздел сообщений VK в этом окне и повторите локальный вход")
	}
	marker := filepath.Join(b.Directory, "accounts", "vk-browser-ready")
	if e := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)), 0600); e != nil {
		return e
	}
	os.Remove(filepath.Join(b.Directory, "accounts", "vk-browser-paused"))
	fmt.Fprintln(os.Stdout, "Профиль браузера VK подготовлен. Реальная отправка ещё не проверена.")
	return nil
}
func vkDialogPeer(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "https") || (u.Hostname() != "vk.com" && u.Hostname() != "vk.ru" && u.Hostname() != "www.vk.com" && u.Hostname() != "m.vk.com") || u.User != nil || u.Port() != "" {
		return "", errors.New("Неподдерживаемая ссылка диалога VK")
	}
	peer := ""
	switch {
	case u.Path == "/im":
		peer = u.Query().Get("sel")
	case strings.HasPrefix(u.Path, "/im/convo/"):
		peer = strings.Trim(strings.TrimPrefix(u.Path, "/im/convo/"), "/")
	case strings.HasPrefix(u.Path, "/write-"):
		peer = "-" + strings.TrimPrefix(u.Path, "/write-")
	}
	id, e := strconv.ParseInt(peer, 10, 64)
	if e != nil || id >= 0 || id == -9223372036854775808 {
		return "", errors.New("Ссылка не ведёт в личный диалог с сообществом")
	}
	return strconv.FormatInt(id, 10), nil
}
func (b VKBrowser) pause() {
	os.WriteFile(filepath.Join(b.Directory, "accounts", "vk-browser-paused"), []byte("Verify the last conversation before re-enabling browser dispatch."), 0600)
}
func (b VKBrowser) Send(ctx context.Context, j Job) (string, error) {
	if e := b.Ready(); e != nil {
		return "", e
	}
	name, e := vkRecipient(j.Contact.Recipient)
	if e != nil {
		return "", e
	}
	target := "https://vk.com/" + name
	if strings.HasPrefix(name, "-") {
		target = "https://vk.com/club" + strings.TrimPrefix(name, "-")
	}
	browser, closeBrowser := b.open(ctx, false)
	defer closeBrowser()
	var raw string
	if e = chromedp.Run(browser, chromedp.Navigate(target), chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Poll(`document.querySelector('a[href*="im?sel=-"],a[href*="/write-"],a[href*="/im/convo/-"]') || document.querySelector('input[type="password"]')`, nil, chromedp.WithPollingTimeout(15*time.Second)), chromedp.Evaluate(vkMessageLinksJS, &raw)); e != nil {
		return "", errors.New("VK: не удалось прочитать сообщество; браузерный адаптер требует проверки на вашем интерфейсе")
	}
	var links []string
	if json.Unmarshal([]byte(raw), &links) != nil {
		return "", errors.New("VK: неизвестный интерфейс сообщества")
	}
	dialog, peer := "", ""
	for _, link := range links {
		candidate, e := vkDialogPeer(link)
		if e != nil {
			continue
		}
		if peer != "" && peer != candidate {
			return "", errors.New("VK: найдено несколько разных адресатов; отправка отменена")
		}
		dialog = link
		peer = candidate
	}
	if dialog == "" {
		return "", errors.New("VK: доступная кнопка сообщения сообществу не найдена; отправка отменена")
	}
	if e = chromedp.Run(browser, chromedp.Navigate(dialog), chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Poll(`document.querySelector('[contenteditable="true"]')`, nil, chromedp.WithPollingTimeout(15*time.Second))); e != nil {
		return "", errors.New("VK: диалог не загрузился; отправка отменена")
	}
	encoded, _ := json.Marshal(peer)
	var ready bool
	if e = chromedp.Run(browser, chromedp.Evaluate("(()=>{if(!("+vkPrepareComposerJS+")("+string(encoded)+"))return false;return document.querySelector('[data-maps-compose=\"1\"]').innerText.trim()==='';})()", &ready)); e != nil || !ready {
		return "", errors.New("VK: не подтверждены адресат и поле сообщения; отправка отменена")
	}
	var before []string
	if e = chromedp.Run(browser, chromedp.Evaluate(vkOutgoingIDsJS, &before)); e != nil {
		return "", errors.New("VK: не удалось прочитать состояние диалога")
	}
	if e = chromedp.Run(browser, chromedp.Focus(`[data-maps-compose="1"]`, chromedp.ByQuery), chromedp.SendKeys(`[data-maps-compose="1"]`, j.Message, chromedp.ByQuery)); e != nil {
		return "", errors.New("VK: текст не введён; отправка отменена")
	}
	// Check the recipient and exact draft again immediately before the only send click.
	encodedText, _ := json.Marshal(j.Message)
	check := `(()=>{const input=document.querySelector('[data-maps-compose="1"]');return input&&input.innerText.trim()===` + string(encodedText) + `&&(` + vkPrepareComposerJS + `)(` + string(encoded) + `,true);})()`
	if e = chromedp.Run(browser, chromedp.Evaluate(check, &ready)); e != nil || !ready {
		return "", errors.New("VK: черновик или адресат изменился; отправка отменена")
	}
	if e = chromedp.Run(browser, chromedp.Click(`[data-maps-send="1"]`, chromedp.ByQuery)); e != nil {
		b.pause()
		return "", errors.New("VK: результат нажатия отправки неизвестен; проверьте диалог. Повтор отключён")
	}
	encodedBefore, _ := json.Marshal(before)
	confirmation := `(` + vkConfirmedMessageJS + `)(` + string(encodedText) + `,` + string(encodedBefore) + `)`
	var id string
	if e = chromedp.Run(browser, chromedp.Poll(confirmation, &id, chromedp.WithPollingTimeout(15*time.Second))); e != nil || id == "" {
		b.pause()
		return "", errors.New("VK: отправка нажата, но новый исходящий номер сообщения не подтверждён; проверьте диалог. Повтор отключён")
	}
	return "browser:" + id, nil
}

const vkMessageLinksJS = `JSON.stringify([...document.querySelectorAll('#page_actions a[href],.page_actions a[href],.group_actions a[href],[data-testid="group-actions"] a[href]')].filter(a=>a.getClientRects().length&&/^(написать сообщение|сообщение|написать)$/i.test(a.innerText.trim())).map(a=>a.href))`
const vkPrepareComposerJS = `(peer,requireEnabled=false,href=location.href)=>{
 const visible=n=>n.getClientRects().length>0;
 const u=new URL(href);const uriPeer=u.searchParams.get('sel')||(u.pathname.match(/^\/im\/convo\/(-\d+)/)||[])[1]||(u.pathname.match(/^\/write(-\d+)$/)||[])[1];
 const activePeers=[...new Set([...document.querySelectorAll('#im_page_wrap[data-peer],.im-page--chat-header[data-peer],.im-page--chat-body[data-peer],[data-testid="conversation"][data-peer]')].filter(visible).map(n=>n.getAttribute('data-peer')))]; if(uriPeer!==peer||activePeers.length!==1||activePeers[0]!==peer)return false;
 const inputs=[...document.querySelectorAll('#im_editable0[contenteditable="true"], [contenteditable="true"][role="textbox"]')].filter(visible);
 const sends=[...document.querySelectorAll('#im_send, button[aria-label="Отправить"], button[data-testid="send_message"]')].filter(visible);
 if(inputs.length!==1||sends.length!==1||(requireEnabled&&sends[0].disabled))return false;
 inputs[0].setAttribute('data-maps-compose','1');sends[0].setAttribute('data-maps-send','1');return true;
}`
const vkOutgoingIDsJS = `[...document.querySelectorAll('.im-mess_out[data-msgid]')].filter(n=>n.getClientRects().length&&/^\d+$/.test(n.getAttribute('data-msgid'))&&Number(n.getAttribute('data-msgid'))>0).map(n=>n.getAttribute('data-msgid'))`
const vkConfirmedMessageJS = `(text,before)=>{const normalize=s=>s.replace(/\s+/g,' ').trim();const n=[...document.querySelectorAll('.im-mess_out[data-msgid]')].find(n=>n.getClientRects().length&&/^\d+$/.test(n.getAttribute('data-msgid'))&&Number(n.getAttribute('data-msgid'))>0&&!before.includes(n.getAttribute('data-msgid'))&&normalize((n.querySelector('.im-mess--text')||n).innerText)===normalize(text));return n?n.getAttribute('data-msgid'):'';}`
