// Command bot is the Telegram front end for the screening API, sold as a
// monthly subscription (docs/DECISIONS.md D26).
//
// SPEC.md §8: "Address in, formatted breakdown out. Mirror the API exactly;
// the bot holds no logic of its own." That still holds for screening: this
// binary speaks HTTP to the API and formats the response, and makes no
// judgement about an address. What it adds is access: who may screen, how
// often, and how they pay. That lives in internal/billing.
//
// Configuration, all from the environment (.env):
//
//	TELEGRAM_BOT_TOKEN       required, from @BotFather
//	TELEGRAM_ADMIN_IDS       comma-separated Telegram user ids with admin rights
//	BILLING_SUPPORT_CONTACT  where customers get help, e.g. @yourname or an email
//	BILLING_USDT_ADDRESS     TRON address receiving USDT; unset disables USDT
//	TRONGRID_API_KEY         used to watch the USDT address
//	APP_ADDR                 where the Mini App and public API listen (default 127.0.0.1:8098)
//	APP_URL                  the public HTTPS URL of the Mini App, e.g. https://risk.example.com/app/;
//	                         unset hides the app button (the server still runs)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/mozer/tether-risk/internal/ingest"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/store"
)

func main() {
	var (
		apiURL      = flag.String("api", "http://127.0.0.1:8099", "screening API base URL")
		chainID     = flag.String("chain", "tron", "default chain")
		configDir   = flag.String("config", "config", "configuration directory")
		concurrency = flag.Int("concurrency", 4, "screens run at the same time")
		tgBase      = flag.String("telegram", "https://api.telegram.org", "Telegram Bot API base URL")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(*apiURL, *chainID, *configDir, *tgBase, *concurrency, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("bot failed", "error", err)
		os.Exit(1)
	}
}

func run(apiURL, chainID, configDir, tgBase string, concurrency int, log *slog.Logger) error {
	// SPEC.md §2: no secrets in the repo, configuration via environment.
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return errors.New("TELEGRAM_BOT_TOKEN is not set")
	}
	admins, err := parseIDs(os.Getenv("TELEGRAM_ADMIN_IDS"))
	if err != nil {
		return fmt.Errorf("TELEGRAM_ADMIN_IDS: %w", err)
	}
	if len(admins) == 0 {
		log.Warn("TELEGRAM_ADMIN_IDS is empty: nobody can grant access, see stats or refund")
	}
	support := strings.TrimSpace(os.Getenv("BILLING_SUPPORT_CONTACT"))
	if support == "" {
		// Telegram requires bots taking payments to offer support.
		return errors.New("BILLING_SUPPORT_CONTACT is not set; Telegram requires a support contact for paid bots")
	}

	bcfg, err := billing.Load(configDir)
	if err != nil {
		return err
	}
	terms := map[string]string{}
	for lang, file := range map[string]string{langEN: "terms.txt", langTR: "terms.tr.txt", langRU: "terms.ru.txt"} {
		raw, err := os.ReadFile(filepath.Join(configDir, file))
		if err != nil {
			return fmt.Errorf("read terms: %w", err)
		}
		terms[lang] = strings.ReplaceAll(string(raw), "{support}", support)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}
	appURL := strings.TrimSpace(os.Getenv("APP_URL"))
	if appURL != "" && !strings.HasPrefix(appURL, "https://") {
		// Telegram opens Mini Apps over HTTPS only.
		return fmt.Errorf("APP_URL must be an https:// URL, got %q", appURL)
	}
	appAddr := os.Getenv("APP_ADDR")
	if appAddr == "" {
		appAddr = "127.0.0.1:8098"
	}

	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()
	// Every TronGrid request counts against the key's daily quota (D35).
	ingest.TrackTronUsage(ctx, pg, log)
	defer ingest.FlushTronUsage(context.WithoutCancel(ctx), pg, log)
	if err := pg.QueryRowContext(ctx, `SELECT 1 FROM bot_users LIMIT 1`).Err(); err != nil &&
		strings.Contains(err.Error(), "does not exist") {
		return errors.New("billing tables missing; run `make migrate`")
	}

	b := &bot{
		tg:        newTelegram(tgBase, token),
		apiURL:    strings.TrimRight(apiURL, "/"),
		chainID:   chainID,
		http:      &http.Client{Timeout: 3 * time.Minute},
		log:       log,
		billing:   bcfg,
		store:     billing.NewStore(pg, bcfg),
		admins:    admins,
		support:   support,
		terms:     terms,
		usdtAddr:  strings.TrimSpace(os.Getenv("BILLING_USDT_ADDRESS")),
		screening: make(chan struct{}, max(concurrency, 1)),
		busy:      map[int64]bool{},
		links:     map[string]string{},
		now:       time.Now,

		chains:      loadChains(cfg),
		appURL:      appURL,
		monitorWake: make(chan struct{}, 1),
		root:        ctx,
		limiter:     &keyLimiter{},
		following:   map[string]bool{},
		sleep:       realSleep,
	}

	if b.usdtAddr != "" {
		if !tron.IsValid(b.usdtAddr) {
			return fmt.Errorf("BILLING_USDT_ADDRESS %q is not a valid TRON address", b.usdtAddr)
		}
		chainCfg, ok := cfg.Chain("tron")
		if !ok {
			return errors.New("tron is not declared in sources.yaml")
		}
		key := os.Getenv("TRONGRID_API_KEY")
		// The watcher needs one request per poll; it takes a small slice of
		// the budget the ingest worker shares.
		b.tron = tron.NewClient(tron.Options{BaseURL: chainCfg.URL, APIKey: key, RequestsPerSecond: 1, Logger: log})
	} else {
		log.Warn("BILLING_USDT_ADDRESS is not set: USDT payments are disabled, Stars only")
	}

	if me, err := b.tg.getMe(ctx); err != nil {
		log.Warn("could not read the bot's username; inline results carry no link to it", "error", err)
	} else {
		b.username = me.Username
	}

	for lang, cmds := range publicCommands {
		codes := []string{lang}
		switch lang {
		case langEN:
			codes = []string{""} // the default for every other language
		case langRU:
			codes = ruClients
		}
		for _, code := range codes {
			if err := b.tg.setMyCommands(ctx, cmds, code); err != nil {
				log.Warn("could not register the command menu", "lang", code, "error", err)
			}
		}
	}
	if appURL != "" {
		if err := b.tg.setMenuButton(ctx, "App", appURL); err != nil {
			log.Warn("could not set the Mini App menu button", "error", err)
		}
	}

	srv := &http.Server{Addr: appAddr, Handler: b.routes(), ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: 4 * time.Minute, IdleTimeout: 2 * time.Minute}

	log.Info("bot starting", "api", b.apiURL, "admins", len(admins), "usdt", b.usdtAddr != "",
		"plans", len(bcfg.Plans), "app_addr", appAddr, "app_url", appURL != "")

	wg := &b.wg
	if b.tron != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.watchUSDT(ctx)
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.monitor(ctx)
	}()
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("app server failed", "addr", appAddr, "error", err)
			stop()
		}
	}()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()

	err = b.poll(ctx, wg)
	wg.Wait()
	return err
}

// bot holds everything a handler needs.
type bot struct {
	tg      *telegram
	apiURL  string
	chainID string
	http    *http.Client
	log     *slog.Logger

	billing  *billing.Config
	store    *billing.Store
	admins   map[int64]bool
	support  string
	terms    map[string]string // by language
	usdtAddr string
	tron     *tron.Client

	// screening bounds concurrent screens: each holds an API request open
	// for up to minutes.
	screening chan struct{}

	mu    sync.Mutex
	busy  map[int64]bool    // users with a screen or batch in flight
	links map[string]string // plan/lang -> cached Stars invoice link

	now func() time.Time

	chains      []chainInfo
	appURL      string        // public Mini App URL; "" when not exposed
	username    string        // the bot's @username, for links from inline results
	monitorWake chan struct{} // nudges the monitor to check new watches now
	root        context.Context
	wg          sync.WaitGroup // background work: batches, watchers, server
	limiter     *keyLimiter
	following   map[string]bool // user/chain/address with a follow-up running
	sleep       func(context.Context, time.Duration) bool
}

// poll reads updates and handles each on its own goroutine. Handling them in
// turn would let one slow screen hold up every other user, and Telegram
// allows only 10 seconds to answer a pre-checkout query.
func (b *bot) poll(ctx context.Context, wg *sync.WaitGroup) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := b.tg.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Code == http.StatusUnauthorized {
				return fmt.Errorf("telegram rejected the bot token: %w", err)
			}
			b.log.Warn("get updates failed", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			wg.Add(1)
			go func(u update) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						b.log.Error("handler panicked", "update", u.UpdateID, "panic", r)
					}
				}()
				b.dispatch(ctx, u)
			}(u)
		}
	}
}

func parseIDs(s string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a Telegram user id", f)
		}
		out[id] = true
	}
	return out, nil
}
