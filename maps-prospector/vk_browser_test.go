package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestVKDialogLinksOnlyTargetCommunities(t *testing.T) {
	for _, raw := range []string{"https://vk.com/im?sel=-123", "https://vk.com/im/convo/-123", "https://vk.com/write-123"} {
		peer, e := vkDialogPeer(raw)
		if e != nil || peer != "-123" {
			t.Errorf("valid community rejected %s: %v", raw, e)
		}
	}
	for _, raw := range []string{"https://vk.com/im?sel=123", "https://vk.com/im?sel=2000000001", "https://evil.example/im?sel=-123", "http://vk.com/im?sel=-123", "https://vk.com/im/convo/-123/other"} {
		if _, e := vkDialogPeer(raw); e == nil {
			t.Errorf("unsafe dialog accepted: %s", raw)
		}
	}
}
func TestVKBrowserLoginAndPauseMarkers(t *testing.T) {
	b := VKBrowser{Directory: t.TempDir()}
	if b.Ready() == nil {
		t.Fatal("unconnected browser accepted")
	}
	dir, e := sessionDirectory(b.Directory)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "vk-browser-ready"), []byte("test"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = b.Ready(); e != nil {
		t.Fatal(e)
	}
	b.pause()
	if b.Ready() == nil {
		t.Fatal("uncertain send not paused")
	}
}
func TestVKBrowserComposerSafetyAndConfirmation(t *testing.T) {
	if os.Getenv("TEST_BROWSER") != "1" {
		t.Skip("set TEST_BROWSER=1 to execute Chromium fixtures")
	}
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath("/usr/bin/chromium"), chromedp.NoSandbox, chromedp.Env("XDG_CONFIG_HOME="+t.TempDir()))
	a, cancel := chromedp.NewExecAllocator(context.Background(), options...)
	defer cancel()
	ctx, closeBrowser := chromedp.NewContext(a)
	defer closeBrowser()
	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()
	html := `<div class="im-page--chat-header" data-peer="-123">Сообщество</div><div id="im_editable0" contenteditable="true" role="textbox" style="height:30px"></div><button id="im_send" disabled>Отправить</button><div class="im-mess_out" data-msgid="10"><div class="im-mess--text">Привет</div></div>`
	encoded, _ := json.Marshal(html)
	if e := chromedp.Run(ctx, chromedp.Evaluate("document.body.innerHTML="+string(encoded), nil)); e != nil {
		t.Fatal(e)
	}
	evalBool := func(script string) bool {
		t.Helper()
		var v bool
		if e := chromedp.Run(ctx, chromedp.Evaluate(script, &v)); e != nil {
			t.Fatal(e)
		}
		return v
	}
	call := func(peer string, enabled bool) string {
		p, _ := json.Marshal(peer)
		mode := "false"
		if enabled {
			mode = "true"
		}
		return "(" + vkPrepareComposerJS + ")(" + string(p) + "," + mode + ",'https://vk.com/im?sel=-123')"
	}
	if !evalBool(call("-123", false)) {
		t.Fatal("empty composer not prepared")
	}
	if evalBool(call("-123", true)) {
		t.Fatal("disabled send accepted")
	}
	if evalBool(call("-999", false)) {
		t.Fatal("wrong recipient accepted")
	}
	if e := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('#im_send').disabled=false; document.querySelector('#im_editable0').innerText='Привет';`, nil)); e != nil {
		t.Fatal(e)
	}
	if !evalBool(call("-123", true)) {
		t.Fatal("valid recipient and enabled composer rejected")
	}
	if e := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('.im-page--chat-header').setAttribute('data-peer','-999');document.body.insertAdjacentHTML('beforeend','<div data-peer="-123">Sidebar contact</div>');`, nil)); e != nil {
		t.Fatal(e)
	}
	if evalBool(call("-123", true)) {
		t.Fatal("sidebar peer substituted for active recipient")
	}
	if e := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('.im-page--chat-header').setAttribute('data-peer','-123')`, nil)); e != nil {
		t.Fatal(e)
	}

	var id string
	confirmation := "(" + vkConfirmedMessageJS + ")('Привет',['10'])"
	if e := chromedp.Run(ctx, chromedp.Evaluate(confirmation, &id)); e != nil {
		t.Fatal(e)
	}
	if id != "" {
		t.Fatal("old message counted as new")
	}
	if e := chromedp.Run(ctx, chromedp.Evaluate(`document.body.insertAdjacentHTML('beforeend','<div class="im-mess_out" data-msgid="11"><div class="im-mess--text">Привет</div></div>')`, nil), chromedp.Evaluate(confirmation, &id)); e != nil {
		t.Fatal(e)
	}
	if id != "11" {
		t.Fatal("new outgoing message not recognized")
	}
}
