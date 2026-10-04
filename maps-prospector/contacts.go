package main

import (
	"net/url"
	"strings"
	"unicode"
)

func normalizePhone(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if unicode.IsDigit(r) && r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	v := b.String()
	if len(v) == 11 && strings.HasPrefix(v, "8") {
		v = "7" + v[1:]
	}
	if len(v) < 8 || len(v) > 15 {
		return ""
	}
	return v
}

// Use exact published links and telephone numbers, never guessed usernames.
func publishedContact(l Lead) (Contact, bool) {
	for _, link := range l.Socials {
		u, e := url.Parse(link)
		if e != nil {
			continue
		}
		if u.Hostname() == "vk.com" || u.Hostname() == "www.vk.com" || u.Hostname() == "m.vk.com" {
			if name, e := vkRecipient(link); e == nil {
				return Contact{l.ID, "vk", name}, true
			}
		}
	}

	for _, link := range l.Socials {
		u, e := url.Parse(link)
		if e != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		path := strings.Trim(u.Path, "/")
		if host == "wa.me" {
			phone := normalizePhone(path)
			if phone != "" {
				return Contact{l.ID, "whatsapp", phone}, true
			}
		}
	}
	for _, link := range l.Socials {
		u, e := url.Parse(link)
		if e != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		name := strings.Trim(u.Path, "/")
		if (host == "t.me" || host == "telegram.me") && !strings.Contains(name, "/") && len(name) >= 5 && len(name) <= 32 && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == "" {
			return Contact{l.ID, "telegram", strings.ToLower(name)}, true
		}
	}
	// The WhatsApp adapter checks whether this published number is registered before sending.
	if phone := normalizePhone(l.Phone); phone != "" {
		return Contact{l.ID, "whatsapp", phone}, true
	}
	return Contact{}, false
}
func (q *Queue) discover(leads []Lead) error {
	for _, l := range leads {
		if l.WebsiteStatus != "not_listed" {
			continue
		}
		exists := false
		for _, c := range q.snapshot().Contacts {
			if c.LeadID == l.ID {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		if c, ok := publishedContact(l); ok {
			if e := q.addContact(c); e != nil {
				return e
			}
		}
	}
	return nil
}
