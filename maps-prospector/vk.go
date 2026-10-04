package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// VK recipient handles are taken from published business links. Resolution is
// performed by VK; usernames are never guessed from a business name.
func vkRecipient(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || (u.Hostname() != "vk.com" && u.Hostname() != "www.vk.com" && u.Hostname() != "m.vk.com") {
			return "", errors.New("Нужна HTTPS-ссылка на сообщество vk.com")
		}
		if u.User != nil || u.Port() != "" {
			return "", errors.New("Некорректная ссылка VK")
		}
		raw = strings.Trim(u.Path, "/")
	}
	if id, e := strconv.ParseInt(raw, 10, 64); e == nil {
		if id >= 0 || id == -9223372036854775808 {
			return "", errors.New("Для сообщества нужен отрицательный peer_id, например -12345")
		}
		return strconv.FormatInt(id, 10), nil
	}
	if len(raw) < 2 || len(raw) > 100 || strings.Trim(raw, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.") != "" {
		return "", errors.New("Нужна ссылка VK, короткое имя сообщества или отрицательный peer_id")
	}
	switch strings.ToLower(raw) {
	case "feed", "im", "friends", "groups", "login", "join", "away.php", "wall", "photo", "video", "market":
		return "", errors.New("Ссылка ведёт на раздел VK, а не на сообщество")
	}
	return strings.ToLower(raw), nil
}
func (c Channels) vkCall(ctx context.Context, method string, params url.Values, out any) error {
	if c.VKToken == "" {
		return errors.New("Нужен пользовательский VK_ACCESS_TOKEN с доступом к сообщениям")
	}
	// Keep the consecutive identity, permission, resolution and send calls below 3 requests/sec.
	timer := time.NewTimer(350 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errors.New("Проверка VK прервана")
	case <-timer.C:
	}
	params.Set("access_token", c.VKToken)
	params.Set("v", "5.199")
	endpoint := c.VKEndpoint
	if endpoint == "" {
		endpoint = "https://api.vk.com/method/"
	}
	data, e := c.request(ctx, strings.TrimRight(endpoint, "/")+"/"+method, "application/x-www-form-urlencoded", params.Encode(), "")
	if e != nil {
		return e
	}
	var envelope struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return errors.New("Некорректный ответ VK")
	}
	if envelope.Error != nil {
		switch envelope.Error.Code {
		case 5, 27, 28:
			return fmt.Errorf("VK: требуется действующий пользовательский токен (код %d)", envelope.Error.Code)
		case 15:
			return errors.New("VK: доступ запрещён; проверьте права пользовательского токена и приложения")
		case 14:
			return errors.New("VK требует CAPTCHA; автоматический повтор отключён")
		case 6, 9:
			return errors.New("VK ограничил запросы; автоматический повтор отключён")
		case 901, 902:
			return fmt.Errorf("VK: сообщество или получатель не разрешает сообщение (код %d)", envelope.Error.Code)
		default:
			return fmt.Errorf("VK отклонил запрос, код %d", envelope.Error.Code)
		}
	}
	if len(envelope.Response) == 0 || string(envelope.Response) == "null" || json.Unmarshal(envelope.Response, out) != nil {
		return errors.New("VK не подтвердил результат операции")
	}
	return nil
}
func (c Channels) checkVKPersonal(ctx context.Context) (int64, error) {
	var users []struct {
		ID int64 `json:"id"`
	}
	if e := c.vkCall(ctx, "users.get", url.Values{}, &users); e != nil {
		return 0, e
	}
	if len(users) != 1 || users[0].ID <= 0 {
		return 0, errors.New("VK: токен личного пользователя не подтверждён; токен сообщества не подходит")
	}
	var permissions int64
	if e := c.vkCall(ctx, "account.getAppPermissions", url.Values{}, &permissions); e != nil {
		return 0, e
	}
	if permissions&4096 == 0 {
		return 0, errors.New("VK: у пользовательского токена нет разрешения messages; наличие личного аккаунта не даёт это право автоматически")
	}
	return users[0].ID, nil
}
func (c Channels) resolveVKGroup(ctx context.Context, recipient string) (int64, error) {
	recipient, e := vkRecipient(recipient)
	if e != nil {
		return 0, e
	}
	if peer, e := strconv.ParseInt(recipient, 10, 64); e == nil {
		return peer, nil
	}
	var object struct {
		Type string `json:"type"`
		ID   int64  `json:"object_id"`
	}
	if e := c.vkCall(ctx, "utils.resolveScreenName", url.Values{"screen_name": {recipient}}, &object); e != nil {
		return 0, e
	}
	if (object.Type != "group" && object.Type != "page") || object.ID <= 0 {
		return 0, errors.New("VK: опубликованная ссылка не подтверждена как сообщество; сообщение не отправлено")
	}
	return -object.ID, nil
}
func (c Channels) sendVK(ctx context.Context, j Job) (string, error) {
	if _, e := c.checkVKPersonal(ctx); e != nil {
		return "", e
	}
	peer, e := c.resolveVKGroup(ctx, j.Contact.Recipient)
	if e != nil {
		return "", e
	}
	var randomID uint32 = 2166136261
	for _, b := range []byte(j.ID) {
		randomID = (randomID ^ uint32(b)) * 16777619
	}
	randomID &= 0x7fffffff
	if randomID == 0 {
		randomID = 1
	}
	var id int64
	e = c.vkCall(ctx, "messages.send", url.Values{"peer_id": {strconv.FormatInt(peer, 10)}, "random_id": {strconv.FormatUint(uint64(randomID), 10)}, "message": {j.Message}}, &id)
	if e != nil {
		return "", e
	}
	if id <= 0 {
		return "", errors.New("VK не подтвердил отправку")
	}
	return strconv.FormatInt(id, 10), nil
}
