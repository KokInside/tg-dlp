package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gotd/log/logslog"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/joho/godotenv"
	"github.com/mdp/qrterminal/v3"
)

var stdin = bufio.NewReader(os.Stdin)

const maxRecipient = 5

func prompt(msg string) string {
	fmt.Print(msg)
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

// ---------- скачивание через yt-dlp ----------

type videoInfo struct {
	Path     string
	Duration float64
	Width    int
	Height   int
}

func downloadVideo(ctx context.Context, url, dir string) (*videoInfo, error) {
	args := []string{
		"-f", "bv*[ext=mp4]+ba[ext=m4a]/b[ext=mp4]/b",
		"--merge-output-format", "mp4",
		"--no-playlist",
		"--progress",
		"--no-simulate",
		"--print", "after_move:%(duration)s",
		"--print", "after_move:%(width)s",
		"--print", "after_move:%(height)s",
		"-o", filepath.Join(dir, "video.%(ext)s"),
	}
	if b := os.Getenv("YTDLP_COOKIES_BROWSER"); b != "" {
		args = append(args, "--cookies-from-browser", b)
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp: %w", err)
	}

	info := &videoInfo{}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 {
		info.Duration, _ = strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	}
	if len(lines) > 1 {
		info.Width, _ = strconv.Atoi(strings.TrimSpace(lines[1]))
	}
	if len(lines) > 2 {
		info.Height, _ = strconv.Atoi(strings.TrimSpace(lines[2]))
	}

	// ищем итоговый файл сами, не полагаясь на путь из вывода yt-dlp
	matches, _ := filepath.Glob(filepath.Join(dir, "video.*"))
	for _, m := range matches {
		if strings.HasSuffix(m, ".mp4") {
			info.Path = m
			break
		}
	}
	if info.Path == "" {
		return nil, fmt.Errorf("mp4 не найден в %s, есть файлы: %q", dir, matches)
	}
	return info, nil
}

// ---------- вход по QR ----------

func ensureAuth(ctx context.Context, client *telegram.Client, loggedIn qrlogin.LoggedIn) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return err
	}
	if status.Authorized {
		return nil
	}

	_, err = client.QR().Auth(ctx, loggedIn, func(ctx context.Context, token qrlogin.Token) error {
		fmt.Println("\nОтсканируйте QR в Telegram на телефоне:")
		fmt.Println("Настройки → Устройства → Подключить устройство\n")
		qrterminal.GenerateHalfBlock(token.URL(), qrterminal.L, os.Stdout)
		return nil
	})
	if err != nil {
		// включён облачный пароль (2FA)
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			if _, err := client.Auth().Password(ctx, prompt("Пароль 2FA: ")); err != nil {
				return fmt.Errorf("2FA: %w", err)
			}
			return nil
		}
		return err
	}
	return nil
}

// ---------- поиск получателя ----------

type candidate struct {
	title string
	peer  tg.InputPeerClass
}

func search(ctx context.Context, api *tg.Client, q string) ([]candidate, error) {
	found, err := api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: q, Limit: 10})
	if err != nil {
		return nil, err
	}

	users := map[int64]*tg.User{}
	for _, u := range found.Users {
		if u, ok := u.(*tg.User); ok {
			users[u.ID] = u
		}
	}
	chats := map[int64]tg.ChatClass{}
	for _, c := range found.Chats {
		switch c := c.(type) {
		case *tg.Chat:
			chats[c.ID] = c
		case *tg.Channel:
			chats[c.ID] = c
		}
	}

	var res []candidate
	seen := map[string]bool{}
	// сначала "мои" результаты (контакты и недавние), потом глобальные
	peers := append(append([]tg.PeerClass{}, found.MyResults...), found.Results...)
	for _, p := range peers {
		var c candidate
		switch p := p.(type) {
		case *tg.PeerUser:
			u, ok := users[p.UserID]
			if !ok {
				continue
			}
			name := strings.TrimSpace(u.FirstName + " " + u.LastName)
			if un, ok := u.GetUsername(); ok {
				name += " (@" + un + ")"
			}
			c = candidate{name, &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}}
		case *tg.PeerChat:
			ch, ok := chats[p.ChatID].(*tg.Chat)
			if !ok {
				continue
			}
			c = candidate{ch.Title + " [группа]", &tg.InputPeerChat{ChatID: ch.ID}}
		case *tg.PeerChannel:
			ch, ok := chats[p.ChannelID].(*tg.Channel)
			if !ok {
				continue
			}
			c = candidate{ch.Title + " [канал/группа]", &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}}
		default:
			continue
		}
		if !seen[c.title] {
			seen[c.title] = true
			res = append(res, c)
		}
	}
	if len(res) > maxRecipient {
		return res[:maxRecipient:maxRecipient], nil
	}
	return res, nil
}

func pickRecipient(ctx context.Context, api *tg.Client) (tg.InputPeerClass, error) {
	for {
		q := prompt("Кому отправить? (имя/username, Enter — выход): ")
		if q == "" {
			return nil, nil
		}
		q = strings.TrimPrefix(q, "@")

		cands, err := search(ctx, api, q)
		if err != nil {
			return nil, err
		}
		if len(cands) == 0 {
			fmt.Println("Никого не нашёл, попробуйте ещё раз.")
			continue
		}
		for i, c := range cands {
			fmt.Printf("  %d. %s\n", i+1, c.title)
		}
		n, err := strconv.Atoi(prompt("Номер (Enter — искать заново): "))
		if err == nil && n >= 1 && n <= len(cands) {
			return cands[n-1].peer, nil
		}
	}
}

// ---------- прогресс загрузки ----------

type progress struct{}

func (progress) Chunk(_ context.Context, s uploader.ProgressState) error {
	if s.Total > 0 {
		fmt.Printf("\rОтправка: %d%%", s.Uploaded*100/s.Total)
	}
	return nil
}

// ---------- main ----------

func main() {
	godotenv.Load()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nОшибка:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	appID, err := strconv.Atoi(os.Getenv("TG_API_ID"))
	if err != nil {
		return errors.New("задайте TG_API_ID")
	}
	appHash := os.Getenv("TG_API_HASH")
	if appHash == "" {
		return errors.New("задайте TG_API_HASH")
	}

	url := ""
	if len(os.Args) > 1 {
		url = os.Args[1]
	} else {
		url = prompt("Ссылка на видео: ")
	}
	if url == "" {
		return errors.New("ссылка не указана")
	}

	// логи в файл (только предупреждения и ошибки; для отладки поставьте slog.LevelDebug)
	logFile, err := os.OpenFile("tgsend.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	slogger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// для QR-входа нужен обработчик апдейтов
	dispatcher := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(dispatcher)

	client := telegram.NewClient(appID, appHash, telegram.Options{
		Logger:         logslog.New(slogger),
		UpdateHandler:  dispatcher,
		SessionStorage: &telegram.FileSessionStorage{Path: "session.json"},
	})

	return client.Run(ctx, func(ctx context.Context) error {
		if err := ensureAuth(ctx, client, loggedIn); err != nil {
			return err
		}

		api := client.API()

		peer, err := pickRecipient(ctx, api)
		if err != nil || peer == nil {
			return err
		}

		dir, err := os.MkdirTemp("", "tgsend-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)

		fmt.Println("Скачиваю...")
		vid, err := downloadVideo(ctx, url, dir)
		if err != nil {
			return err
		}
		fmt.Println("Скачано:", vid.Path)

		u := uploader.NewUploader(api).WithProgress(progress{})
		file, err := u.FromPath(ctx, vid.Path)
		if err != nil {
			return err
		}

		doc := message.UploadedDocument(file).
			MIME("video/mp4").
			Attributes(&tg.DocumentAttributeVideo{
				SupportsStreaming: true,
				Duration:          vid.Duration,
				W:                 vid.Width,
				H:                 vid.Height,
			})

		sender := message.NewSender(api).WithUploader(u)
		if _, err := sender.To(peer).Media(ctx, doc); err != nil {
			return err
		}
		fmt.Println("\nГотово!")
		return nil
	})
}
