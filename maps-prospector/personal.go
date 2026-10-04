package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/mdp/qrterminal/v3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"golang.org/x/term"
	"google.golang.org/protobuf/proto"
)

type PersonalSender struct {
	Directory string
	VK        Channels
	VKBackend string
	VKBrowser VKBrowser
}

func (s PersonalSender) Ready(c Contact) error {
	switch c.Channel {
	case "vk":
		if s.VKBackend == "browser" {
			return s.VKBrowser.Ready()
		}
		if s.VK.VKToken == "" {
			return errors.New("VK: ожидается VK_ACCESS_TOKEN")
		}
	case "telegram":
		if _, e := telegramClient(s.Directory); e != nil {
			return e
		}
		if _, e := os.Stat(filepath.Join(s.Directory, "accounts", "telegram.session")); e != nil {
			return errors.New("Telegram: ожидается локальный вход --login telegram")
		}
	case "whatsapp":
		if _, e := os.Stat(filepath.Join(s.Directory, "accounts", "whatsapp.db")); e != nil {
			return errors.New("WhatsApp: ожидается локальный вход --login whatsapp")
		}
		client, db, e := whatsAppClient(context.Background(), s.Directory)
		if e != nil {
			return e
		}
		defer db.Close()
		if client.Store.ID == nil {
			return errors.New("WhatsApp: ожидается привязка устройства --login whatsapp")
		}
	default:
		return errors.New("Неизвестный канал")
	}
	return nil
}

func (s PersonalSender) Send(ctx context.Context, j Job) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	switch j.Contact.Channel {
	case "vk":
		if s.VKBackend == "browser" {
			return s.VKBrowser.Send(ctx, j)
		}
		return s.VK.Send(ctx, j)
	case "telegram":
		return s.sendTelegram(ctx, j)
	case "whatsapp":
		return s.sendWhatsApp(ctx, j)
	default:
		return "", errors.New("Неизвестный канал")
	}
}
func sessionDirectory(base string) (string, error) {
	path := filepath.Join(base, "accounts")
	if e := os.MkdirAll(path, 0700); e != nil {
		return "", e
	}
	return path, nil
}
func telegramClient(base string) (*telegram.Client, error) {
	id, e := strconv.Atoi(os.Getenv("TELEGRAM_API_ID"))
	hash := os.Getenv("TELEGRAM_API_HASH")
	if e != nil || id <= 0 || hash == "" {
		return nil, errors.New("Нужны TELEGRAM_API_ID и TELEGRAM_API_HASH приложения с my.telegram.org")
	}
	dir, e := sessionDirectory(base)
	if e != nil {
		return nil, e
	}
	return telegram.NewClient(id, hash, telegram.Options{SessionStorage: &session.FileStorage{Path: filepath.Join(dir, "telegram.session")}}), nil
}
func (s PersonalSender) sendTelegram(ctx context.Context, j Job) (string, error) {
	client, e := telegramClient(s.Directory)
	if e != nil {
		return "", e
	}
	result := ""
	e = client.Run(ctx, func(ctx context.Context) error {
		state, e := client.Auth().Status(ctx)
		if e != nil || !state.Authorized {
			return errors.New("Telegram: требуется локальный вход --login telegram")
		}
		resolved, e := client.API().ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: strings.TrimPrefix(j.Contact.Recipient, "@")})
		if e != nil {
			return errors.New("Telegram: контакт не найден или недоступен")
		}
		peer, ok := resolved.Peer.(*tg.PeerUser)
		if !ok {
			return errors.New("Telegram: контакт является каналом или группой; личное обращение не отправлено")
		}
		var input *tg.InputPeerUser
		for _, entry := range resolved.Users {
			if user, ok := entry.(*tg.User); ok && user.ID == peer.UserID {
				input = &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}
			}
		}
		if input == nil {
			return errors.New("Telegram: не удалось подтвердить получателя")
		}
		sum := sha256.Sum256([]byte("telegram:" + j.ID))
		randomID := int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
		if randomID == 0 {
			randomID = 1
		}
		_, e = client.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: input, Message: j.Message, RandomID: randomID, NoWebpage: true})
		if e != nil {
			return errors.New("Telegram не подтвердил отправку; проверьте ограничения аккаунта и историю сообщений")
		}
		result = fmt.Sprintf("request:%d", randomID)
		return nil
	})
	if e != nil {
		return "", errors.New("Telegram: отправка не подтверждена; проверьте локальный вход, контакт и историю сообщений")
	}
	return result, nil
}
func whatsAppClient(ctx context.Context, base string) (*whatsmeow.Client, *sqlstore.Container, error) {
	dir, e := sessionDirectory(base)
	if e != nil {
		return nil, nil, e
	}
	dbPath := filepath.Join(dir, "whatsapp.db")
	f, e := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, nil, e
	}
	f.Close()
	container, e := sqlstore.New(ctx, "sqlite3", "file:"+filepath.ToSlash(dbPath)+"?_pragma=foreign_keys(1)", nil)
	if e != nil {
		return nil, nil, errors.New("Не удалось открыть хранилище сессии WhatsApp")
	}
	device, e := container.GetFirstDevice(ctx)
	if e != nil {
		container.Close()
		return nil, nil, errors.New("Не удалось прочитать устройство WhatsApp")
	}
	return whatsmeow.NewClient(device, nil), container, nil
}
func (s PersonalSender) sendWhatsApp(ctx context.Context, j Job) (string, error) {
	client, container, e := whatsAppClient(ctx, s.Directory)
	if e != nil {
		return "", e
	}
	defer container.Close()
	defer client.Disconnect()
	if client.Store.ID == nil {
		return "", errors.New("WhatsApp: сначала выполните --login whatsapp и привяжите устройство через QR")
	}
	if e = client.ConnectContext(ctx); e != nil {
		return "", errors.New("WhatsApp: не удалось подключить привязанное устройство")
	}
	found, e := client.IsOnWhatsApp(ctx, []string{"+" + j.Contact.Recipient})
	if e != nil || len(found) != 1 || !found[0].IsIn {
		return "", errors.New("WhatsApp: номер не подтверждён как доступный контакт")
	}
	jid := types.NewJID(j.Contact.Recipient, types.DefaultUserServer)
	sum := sha256.Sum256([]byte("whatsapp:" + j.ID))
	id := strings.ToUpper(hex.EncodeToString(sum[:10]))
	res, e := client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(j.Message)}, whatsmeow.SendRequestExtra{ID: id, Timeout: 45 * time.Second})
	if e != nil || res.ID == "" {
		return "", errors.New("WhatsApp: отправка не подтверждена; проверьте историю сообщений перед повтором")
	}
	return res.ID, nil
}

type terminalAuth struct{}

func hiddenInput(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("Вход разрешён только в локальном интерактивном терминале")
	}
	fmt.Fprint(os.Stdout, prompt)
	value, e := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(value)), nil
}
func (terminalAuth) Phone(context.Context) (string, error) {
	return hiddenInput("Номер Telegram (международный формат, ввод скрыт): ")
}
func (terminalAuth) Password(context.Context) (string, error) {
	return hiddenInput("Пароль 2FA Telegram (ввод скрыт): ")
}
func (terminalAuth) Code(context.Context, *tg.AuthSentCode) (string, error) {
	return hiddenInput("Код входа Telegram (ввод скрыт): ")
}
func (terminalAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("Регистрация новых аккаунтов не поддерживается")
}
func (terminalAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("Регистрация новых аккаунтов не поддерживается")
}
func loginPersonal(ctx context.Context, base, channel string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("Запустите вход на своём компьютере в интерактивном терминале; коды и QR не отправляйте в чат")
	}
	switch channel {
	case "telegram":
		client, e := telegramClient(base)
		if e != nil {
			return e
		}
		e = client.Run(ctx, func(ctx context.Context) error {
			return client.Auth().IfNecessary(ctx, auth.NewFlow(terminalAuth{}, auth.SendCodeOptions{}))
		})
		if e != nil {
			return errors.New("Вход Telegram не завершён; проверьте настройки приложения и код в локальном терминале")
		}
		fmt.Fprintln(os.Stdout, "Telegram подключён. Сессия хранится локально в data/accounts.")
		return nil
	case "whatsapp":
		client, container, e := whatsAppClient(ctx, base)
		if e != nil {
			return e
		}
		defer container.Close()
		defer client.Disconnect()
		if client.Store.ID != nil {
			fmt.Fprintln(os.Stdout, "WhatsApp уже привязан.")
			return nil
		}
		qr, e := client.GetQRChannel(ctx)
		if e != nil {
			return errors.New("Не удалось подготовить привязку WhatsApp")
		}
		if e = client.ConnectContext(ctx); e != nil {
			return errors.New("Не удалось подключиться к WhatsApp")
		}
		for event := range qr {
			switch event.Event {
			case "code":
				fmt.Fprintln(os.Stdout, "WhatsApp → Связанные устройства → Привязка устройства. QR только для владельца аккаунта:")
				qrterminal.GenerateHalfBlock(event.Code, qrterminal.L, os.Stdout)
			case "success":
				fmt.Fprintln(os.Stdout, "WhatsApp подключён.")
				return nil
			case "timeout":
				return errors.New("Время QR истекло; запустите вход заново")
			}
		}
		return errors.New("Привязка WhatsApp не завершена")
	default:
		return errors.New("--login принимает telegram или whatsapp")
	}
}
