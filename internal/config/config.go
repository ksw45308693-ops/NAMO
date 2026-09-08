package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type LookupFunc func(string) (string, bool)

type Config struct {
	DatabaseURL          string
	MigrationDatabaseURL string
	G2BAPIKey            string
	G2BAPIKeyFile        string
	SMTPHost             string
	SMTPPort             int
	SMTPUser             string
	SMTPPassword         string
	SMTPFrom             string
	BaseURL              string
	SessionKey           string
	ListenAddr           string
	TimeZone             string
	DeliveryMode         string
	ReportDir            string
}

func Load(lookup LookupFunc) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	cfg := Config{
		DatabaseURL:          value(lookup, "DATABASE_URL", ""),
		MigrationDatabaseURL: value(lookup, "MIGRATION_DATABASE_URL", ""),
		G2BAPIKey:            value(lookup, "G2B_API_KEY", ""),
		G2BAPIKeyFile:        value(lookup, "G2B_API_KEY_FILE", defaultAPIKeyFile()),
		SMTPHost:             value(lookup, "SMTP_HOST", ""),
		SMTPUser:             value(lookup, "SMTP_USER", ""),
		SMTPPassword:         value(lookup, "SMTP_PASSWORD", ""),
		SMTPFrom:             value(lookup, "SMTP_FROM", ""),
		BaseURL:              value(lookup, "BASE_URL", "http://127.0.0.1:8080"),
		SessionKey:           value(lookup, "SESSION_KEY", ""),
		ListenAddr:           value(lookup, "LISTEN_ADDR", "127.0.0.1:8080"),
		TimeZone:             value(lookup, "TIME_ZONE", "Asia/Seoul"),
		DeliveryMode:         value(lookup, "DELIVERY_MODE", "report"),
		ReportDir:            value(lookup, "REPORT_DIR", ""),
	}
	port, err := strconv.Atoi(value(lookup, "SMTP_PORT", "587"))
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("SMTP_PORT must be a number from 1 to 65535")
	}
	cfg.SMTPPort = port
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if len(cfg.SessionKey) < 32 {
		return Config{}, fmt.Errorf("SESSION_KEY must contain at least 32 characters")
	}
	if parsed, err := url.Parse(cfg.BaseURL); err != nil || !validBaseURL(parsed, false) {
		return Config{}, fmt.Errorf("BASE_URL must be an absolute HTTP(S) origin without credentials, path, query, or fragment")
	}
	return cfg, nil
}

func (c Config) ValidateCommand(command string) error {
	switch command {
	case "serve":
		if err := c.validateReportDelivery(command); err != nil {
			return err
		}
		baseURL, err := url.Parse(c.BaseURL)
		if err != nil || !validBaseURL(baseURL, true) {
			return fmt.Errorf("BASE_URL must be an absolute HTTPS origin for serve")
		}
		if err := validateLoopbackListener(c.ListenAddr); err != nil {
			return err
		}
	case "collect-once":
		// The current key is resolved from the private file at collection time.
	case "send-test-mail":
		if strings.TrimSpace(c.SMTPHost) == "" {
			return fmt.Errorf("SMTP_HOST is required for send-test-mail")
		}
		if !validPlainMailbox(c.SMTPFrom) {
			return fmt.Errorf("SMTP_FROM must be a plain email address for send-test-mail")
		}
	case "generate-report":
		if err := c.validateReportDelivery(command); err != nil {
			return err
		}
	case "migrate", "create-admin":
		if strings.TrimSpace(c.MigrationDatabaseURL) == "" {
			return fmt.Errorf("MIGRATION_DATABASE_URL is required for %s", command)
		}
		if sameDatabaseRole(c.MigrationDatabaseURL, c.DatabaseURL) {
			return fmt.Errorf("MIGRATION_DATABASE_URL must use different owner credentials from DATABASE_URL")
		}
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	return nil
}

func (c Config) validateReportDelivery(command string) error {
	if c.DeliveryMode != "report" {
		return fmt.Errorf("DELIVERY_MODE must be report for %s", command)
	}
	directory := c.ReportDir
	if directory == "" || !filepath.IsAbs(directory) || hasParentDirectory(directory) {
		return fmt.Errorf("REPORT_DIR must be a safe absolute directory for %s", command)
	}
	directory = filepath.Clean(directory)
	if filepath.Dir(directory) == directory {
		return fmt.Errorf("REPORT_DIR must not be a filesystem root for %s", command)
	}
	return nil
}

func hasParentDirectory(path string) bool {
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || os.IsPathSeparator(path[index]) {
			if path[start:index] == ".." {
				return true
			}
			start = index + 1
		}
	}
	return false
}

func validPlainMailbox(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func validateLoopbackListener(address string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("LISTEN_ADDR must be a loopback host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("LISTEN_ADDR must use a loopback address and port from 1 to 65535")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("LISTEN_ADDR must use a loopback address")
	}
	return nil
}

func validBaseURL(parsed *url.URL, requireHTTPS bool) bool {
	if parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return false
	}
	if requireHTTPS {
		if !strings.EqualFold(parsed.Scheme, "https") {
			return false
		}
	} else if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	path := parsed.EscapedPath()
	return path == "" || path == "/"
}

func sameDatabaseRole(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == right {
		return true
	}
	a, aErr := url.Parse(left)
	b, bErr := url.Parse(right)
	if aErr != nil || bErr != nil || a.Scheme == "" || b.Scheme == "" || a.User == nil || b.User == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Host, b.Host) &&
		a.Path == b.Path &&
		a.User.Username() == b.User.Username()
}

func value(lookup LookupFunc, key, fallback string) string {
	if v, ok := lookup(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}
