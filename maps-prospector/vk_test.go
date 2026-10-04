package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestVKPersonalSendResolvesCommunity(t *testing.T) {
	sent := 0
	var randomID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.ParseForm(); e != nil {
			t.Error(e)
		}
		if r.PostForm.Get("access_token") != "test-token" {
			t.Error("missing token")
		}
		switch r.URL.Path {
		case "/users.get":
			fmt.Fprint(w, `{"response":[{"id":42}]}`)
		case "/account.getAppPermissions":
			fmt.Fprint(w, `{"response":4096}`)
		case "/utils.resolveScreenName":
			if r.Form.Get("screen_name") != "business_name" {
				t.Error("wrong screen name")
			}
			fmt.Fprint(w, `{"response":{"type":"group","object_id":123}}`)
		case "/messages.send":
			sent++
			if r.Form.Get("peer_id") != "-123" || r.Form.Get("message") != outreach {
				t.Error("wrong recipient or message")
			}
			if r.Form.Get("random_id") == "" || r.Form.Get("random_id") == "0" {
				t.Error("missing dedup id")
			}
			if randomID != "" && randomID != r.Form.Get("random_id") {
				t.Error("unstable dedup id")
			}
			randomID = r.Form.Get("random_id")
			fmt.Fprint(w, `{"response":777}`)
		default:
			t.Error("unexpected method")
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	c := Channels{Client: server.Client(), VKToken: "test-token", VKEndpoint: server.URL}
	for i := 0; i < 2; i++ {
		id, e := c.Send(context.Background(), Job{ID: "lead-1", Contact: Contact{Channel: "vk", Recipient: "https://vk.com/business_name"}, Message: outreach})
		if e != nil || id != "777" {
			t.Fatal("send failed", e)
		}
	}
	if sent != 2 {
		t.Fatal("mock methods not exercised")
	}
}
func TestVKRefusesGroupTokensMissingPermissionsAndUserRecipients(t *testing.T) {
	cases := []struct {
		Name                           string
		Users, Permissions, Resolution string
	}{
		{"community token", `{"error":{"error_code":28,"error_msg":"secret-token"}}`, `{"response":4096}`, `{"response":{"type":"group","object_id":123}}`},
		{"missing messages permission", `{"response":[{"id":42}]}`, `{"response":0}`, `{"response":{"type":"group","object_id":123}}`},
		{"user instead of community", `{"response":[{"id":42}]}`, `{"response":4096}`, `{"response":{"type":"user","object_id":123}}`},
	}
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			sent := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/users.get":
					fmt.Fprint(w, tt.Users)
				case "/account.getAppPermissions":
					fmt.Fprint(w, tt.Permissions)
				case "/utils.resolveScreenName":
					fmt.Fprint(w, tt.Resolution)
				case "/messages.send":
					sent++
					fmt.Fprint(w, `{"response":1}`)
				}
			}))
			defer server.Close()
			c := Channels{Client: server.Client(), VKToken: "secret-token", VKEndpoint: server.URL}
			_, e := c.Send(context.Background(), Job{ID: "1", Contact: Contact{Channel: "vk", Recipient: "business_name"}, Message: outreach})
			if e == nil || sent != 0 || strings.Contains(e.Error(), "secret-token") {
				t.Fatal("failed closed check or leaked token")
			}
		})
	}
}
func TestVKPublishedLinkAndValidation(t *testing.T) {
	c, ok := publishedContact(Lead{ID: "1", Socials: []string{"https://vk.com/business_name"}})
	if !ok || c.Channel != "vk" || c.Recipient != "business_name" {
		t.Fatal("VK contact not discovered")
	}
	q := &Queue{Path: filepath.Join(t.TempDir(), "queue.json")}
	if e := q.addContact(c); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{"123", "https://vk.com.evil.example/name", "https://vk.com/im", "https://vk.com/wall-123_456", "https://vk.com/name/extra", "https://user@vk.com/name"} {
		if _, e := vkRecipient(raw); e == nil {
			t.Errorf("invalid recipient accepted: %s", raw)
		}
	}
	if peer, e := vkRecipient("-123"); e != nil || peer != "-123" {
		t.Fatal("known community ID rejected")
	}
}
func TestVKAPIErrorDoesNotReportSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.get":
			fmt.Fprint(w, `{"response":[{"id":42}]}`)
		case "/account.getAppPermissions":
			fmt.Fprint(w, `{"response":4096}`)
		case "/messages.send":
			fmt.Fprint(w, `{"error":{"error_code":901,"error_msg":"secret-token"}}`)
		}
	}))
	defer server.Close()
	c := Channels{Client: server.Client(), VKToken: "secret-token", VKEndpoint: server.URL}
	id, e := c.Send(context.Background(), Job{ID: "1", Contact: Contact{Channel: "vk", Recipient: "-123"}, Message: outreach})
	if e == nil || id != "" || strings.Contains(e.Error(), "secret-token") {
		t.Fatal("provider rejection mishandled")
	}
}
