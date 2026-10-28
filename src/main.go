package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ProtonMail/go-proton-api"
	"github.com/go-resty/resty/v2"
)

// bridgeVersion is the Proton Mail Bridge release whose API client and app version this build follows.
const bridgeVersion = "3.27.1"

var version = "dev"

// maxMessageSize is the SIZE that Proton Mail's SMTP server announces.
const maxMessageSize = 36480000

type config struct {
	listen     string
	username   string
	password   string
	dataDir    string
	maxSize    int64
	submission submission

	checkTimeout    time.Duration
	deliveryTimeout time.Duration
	removalWaits    []time.Duration
	checkEvery      time.Duration
	retryEvery      time.Duration
}

const usage = `Usage: proton_mail_forwarder [command]

Commands:
  serve        accept mail over SMTP and send it to the inbox (default)
  login        sign in to Proton Mail
  logout       sign out and delete the session
  healthcheck  fail while mail cannot be delivered or a copy stays under Sent
  version      print the version
  help         print this help
`

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "version", "--version":
		fmt.Println(version)
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	case "serve", "login", "logout", "healthcheck":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		os.Exit(2)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	if command == "serve" || command == "login" {
		if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
			log.Fatal(err)
		}
		if err := writable(cfg.dataDir); err != nil {
			log.Fatal(err)
		}
	}

	switch command {
	case "serve":
		err = runServe(cfg)
	case "login":
		err = login(context.Background(), cfg, newManager(), newTerminalPrompter())
	case "logout":
		err = logout(context.Background(), cfg, newManager())
	case "healthcheck":
		err = healthcheck(cfg)
	}

	if err != nil {
		log.Fatal(err)
	}
}

func runServe(cfg config) error {
	if cfg.submission.token == "" {
		return errors.New("set PROTON_SMTP_USERNAME and PROTON_SMTP_TOKEN")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg, newManager())
}

func loadConfig() (config, error) {
	cfg := config{
		listen:   envOr("SMTP_LISTEN", "127.0.0.1:2525"),
		username: os.Getenv("SMTP_USERNAME"),
		password: os.Getenv("SMTP_PASSWORD"),
		dataDir:  os.Getenv("DATA_DIR"),
		maxSize:  25 << 20,
		submission: submission{
			addr:   "smtp.protonmail.ch:587",
			sender: os.Getenv("PROTON_SMTP_USERNAME"),
			token:  os.Getenv("PROTON_SMTP_TOKEN"),
		},

		checkTimeout:    time.Minute,
		deliveryTimeout: 2 * time.Minute,
		removalWaits:    []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute},
		checkEvery:      checkInterval,
		retryEvery:      time.Minute,
	}

	if cfg.dataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return cfg, fmt.Errorf("set DATA_DIR: %w", err)
		}
		cfg.dataDir = filepath.Join(dir, "proton_mail_forwarder")
	}

	if (cfg.username == "") != (cfg.password == "") {
		return cfg, errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together")
	}
	if (cfg.submission.sender == "") != (cfg.submission.token == "") {
		return cfg, errors.New("PROTON_SMTP_USERNAME and PROTON_SMTP_TOKEN must be set together")
	}
	if cfg.submission.sender != "" && !strings.Contains(cfg.submission.sender, "@") {
		return cfg, errors.New("PROTON_SMTP_USERNAME must be the address the token belongs to")
	}

	if raw := os.Getenv("MAX_MESSAGE_SIZE"); raw != "" {
		size, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || size <= 0 || size > maxMessageSize {
			return cfg, fmt.Errorf("MAX_MESSAGE_SIZE must be a number of bytes between 1 and %d", maxMessageSize)
		}
		cfg.maxSize = size
	}

	return cfg, nil
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".check-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	f.Close()
	return os.Remove(f.Name())
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func newManager(opts ...proton.Option) *proton.Manager {
	m := proton.New(append([]proton.Option{proton.WithAppVersion("linux-bridge@" + bridgeVersion), proton.WithLogger(quiet{})}, opts...)...)
	m.AddPreRequestHook(func(_ *resty.Client, r *resty.Request) error {
		r.SetHeader("User-Agent", "proton_mail_forwarder/"+version)
		return nil
	})
	return m
}

type quiet struct{}

func (quiet) Errorf(string, ...any) {}
func (quiet) Warnf(string, ...any)  {}
func (quiet) Debugf(string, ...any) {}

func sessionPath(cfg config) string { return filepath.Join(cfg.dataDir, "session.json") }

func statusPath(cfg config) string { return filepath.Join(cfg.dataDir, "status") }

func copiesPath(cfg config) string { return filepath.Join(cfg.dataDir, "sent-copies.json") }

func healthcheck(cfg config) error {
	info, err := os.Stat(statusPath(cfg))
	if err != nil {
		return fmt.Errorf("no status yet: %w", err)
	}
	status, err := os.ReadFile(statusPath(cfg))
	if err != nil {
		return err
	}
	if text := strings.TrimSpace(string(status)); text != statusOK {
		return errors.New(text)
	}
	if age := time.Since(info.ModTime()); age > 2*checkInterval {
		return fmt.Errorf("no check for %v", age.Round(time.Minute))
	}

	host, port, err := net.SplitHostPort(cfg.listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}
