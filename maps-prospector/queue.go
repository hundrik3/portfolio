package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A recipient must be supplied from a verified business contact. Never infer a
// Telegram ID, VK peer ID or WhatsApp number from a business name.
type Contact struct {
	LeadID    string `json:"lead_id"`
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
}
type Job struct {
	ID          string    `json:"id"`
	Contact     Contact   `json:"contact"`
	Message     string    `json:"message"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	ProviderID  string    `json:"provider_id,omitempty"`
	Error       string    `json:"error,omitempty"`
}
type QueueState struct {
	Contacts []Contact `json:"contacts"`
	Jobs     []Job     `json:"jobs"`
}
type Queue struct {
	mu    sync.Mutex
	Path  string
	State QueueState
}

func (q *Queue) load() error {
	b, e := os.ReadFile(q.Path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &q.State); e != nil {
		return e
	}
	for i := range q.State.Jobs {
		if q.State.Jobs[i].Status == "sending" {
			q.State.Jobs[i].Status = "uncertain"
			q.State.Jobs[i].Error = "Процесс завершился во время отправки; проверьте историю аккаунта. Автоматический повтор отключён."
		}
	}
	return q.save()
}
func (q *Queue) save() error {
	b, e := json.MarshalIndent(q.State, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(q.Path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(q.Path), ".queue-*")
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
	return os.Rename(f.Name(), q.Path)
}
func (q *Queue) snapshot() QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueueState{append([]Contact(nil), q.State.Contacts...), append([]Job(nil), q.State.Jobs...)}
}
func (q *Queue) addContact(c Contact) error {
	if c.LeadID == "" || c.Recipient == "" {
		return errors.New("Нужны ID бизнеса и проверенный контакт")
	}
	switch c.Channel {
	case "vk":
		recipient, e := vkRecipient(c.Recipient)
		if e != nil {
			return e
		}
		c.Recipient = recipient
	case "telegram":
		name := strings.TrimPrefix(c.Recipient, "@")
		if len(name) < 5 || len(name) > 32 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
			return errors.New("Нужен Telegram username личного контакта, без ссылки")
		}
		c.Recipient = strings.ToLower(name)
	case "whatsapp":
		if len(c.Recipient) < 8 || len(c.Recipient) > 15 || strings.Trim(c.Recipient, "0123456789") != "" {
			return errors.New("Нужен международный номер WhatsApp из 8–15 цифр без плюса")
		}
	default:
		return errors.New("Неизвестный канал")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	prev := append([]Contact(nil), q.State.Contacts...)
	for i := range q.State.Contacts {
		if q.State.Contacts[i].LeadID == c.LeadID {
			q.State.Contacts[i] = c
			if e := q.save(); e != nil {
				q.State.Contacts = prev
				return e
			}
			return nil
		}
	}
	q.State.Contacts = append(q.State.Contacts, c)
	if e := q.save(); e != nil {
		q.State.Contacts = prev
		return e
	}
	return nil
}
func (q *Queue) enqueue(leads []Lead) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	prev := append([]Job(nil), q.State.Jobs...)
	for _, lead := range leads {
		if lead.WebsiteStatus != "not_listed" {
			continue
		}
		already := false
		for _, j := range q.State.Jobs {
			if j.Contact.LeadID == lead.ID {
				already = true
				break
			}
		}
		if already {
			continue
		}
		for _, c := range q.State.Contacts {
			if c.LeadID == lead.ID {
				recipientUsed := false
				for _, job := range q.State.Jobs {
					if job.Contact.Channel == c.Channel && job.Contact.Recipient == c.Recipient {
						recipientUsed = true
						break
					}
				}
				if recipientUsed {
					break
				}
				q.State.Jobs = append(q.State.Jobs, Job{ID: lead.ID, Contact: c, Message: outreach, Status: "pending", CreatedAt: time.Now().UTC()})
				break
			}
		}
	}
	if e := q.save(); e != nil {
		q.State.Jobs = prev
		return e
	}
	return nil
}

type Sender interface {
	Send(context.Context, Job) (string, error)
}

// No automatic retries: a timeout may occur after the provider accepted a message.
func (q *Queue) tick(ctx context.Context, s Sender, leads []Lead, limit int) error {
	q.mu.Lock()
	count := 0
	today := time.Now().UTC().Format("2006-01-02")
	for _, j := range q.State.Jobs {
		if !j.AttemptedAt.IsZero() && j.AttemptedAt.UTC().Format("2006-01-02") == today {
			count++
		}
	}
	if count >= limit {
		q.mu.Unlock()
		return nil
	}
	idx := -1
	for i, j := range q.State.Jobs {
		if j.Status == "pending" {
			eligible := false
			for _, l := range leads {
				if l.ID == j.Contact.LeadID && l.WebsiteStatus == "not_listed" {
					eligible = true
					break
				}
			}
			if eligible {
				if ready, ok := s.(interface{ Ready(Contact) error }); ok {
					if e := ready.Ready(j.Contact); e != nil {
						q.State.Jobs[i].Error = e.Error()
						continue
					}
				}
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		e := q.save()
		q.mu.Unlock()
		return e
	}
	prev := q.State.Jobs[idx]
	q.State.Jobs[idx].Status = "sending"
	q.State.Jobs[idx].AttemptedAt = time.Now().UTC()
	if e := q.save(); e != nil {
		q.State.Jobs[idx] = prev
		q.mu.Unlock()
		return e
	}
	job := q.State.Jobs[idx]
	q.mu.Unlock()
	id, e := s.Send(ctx, job)
	q.mu.Lock()
	defer q.mu.Unlock()
	if e != nil {
		q.State.Jobs[idx].Status = "uncertain"
		q.State.Jobs[idx].Error = e.Error()
	} else {
		q.State.Jobs[idx].Status = "sent"
		q.State.Jobs[idx].ProviderID = id
	}
	return q.save()
}

type Channels struct {
	Client              *http.Client
	VKToken, VKEndpoint string
}

func (c Channels) Send(ctx context.Context, j Job) (string, error) {
	if j.Contact.Channel != "vk" {
		return "", errors.New("Неизвестный канал")
	}
	return c.sendVK(ctx, j)
}
func (c Channels) request(ctx context.Context, endpoint, contentType, body, bearer string) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if e != nil {
		return nil, errors.New("Некорректная конфигурация канала")
	}
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, e := c.Client.Do(req)
	if e != nil {
		return nil, errors.New("Ошибка связи с каналом; результат отправки неизвестен. Проверьте историю сообщений")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if e != nil {
		return nil, errors.New("Не удалось прочитать ответ канала; проверьте историю сообщений")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("Канал вернул HTTP %d; проверьте права аккаунта и ограничения API", res.StatusCode)
	}
	return b, nil
}
