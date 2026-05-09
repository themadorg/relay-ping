package chatmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Account struct {
	Username    string
	Password    string
	SMTPAddress string
	IMAPAddress string
}

type rawAccount struct {
	Username string `json:"username"`
	User     string `json:"user"`
	Email    string `json:"email"`

	Password string `json:"password"`
	Pass     string `json:"pass"`

	SMTPAddress string `json:"smtp"`
	SMTPServer  string `json:"smtp_server"`
	SMTPHost    string `json:"smtp_host"`
	SMTPPort    string `json:"smtp_port"`

	IMAPAddress string `json:"imap"`
	IMAPServer  string `json:"imap_server"`
	IMAPHost    string `json:"imap_host"`
	IMAPPort    string `json:"imap_port"`
}

func EndpointFromDomain(domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", fmt.Errorf("domain is empty")
	}
	u, err := url.Parse(domain)
	if err != nil {
		return "", fmt.Errorf("invalid domain: %w", err)
	}
	if u.Scheme == "" {
		u, err = url.Parse("https://" + domain)
		if err != nil {
			return "", fmt.Errorf("invalid domain: %w", err)
		}
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid domain host")
	}
	u.Path = "/new"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func DefaultMailAddrsFromDomain(domain string) (smtpAddr, imapAddr string, err error) {
	domain = strings.TrimSpace(domain)
	u, parseErr := url.Parse(domain)
	if parseErr != nil {
		return "", "", fmt.Errorf("invalid domain: %w", parseErr)
	}
	if u.Scheme == "" {
		u, parseErr = url.Parse("https://" + domain)
		if parseErr != nil {
			return "", "", fmt.Errorf("invalid domain: %w", parseErr)
		}
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("invalid domain host")
	}
	host := u.Hostname()
	return host + ":587", host + ":993", nil
}

func FetchAccount(ctx context.Context, endpoint string, logWriter io.Writer, verbosity int) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return Account{}, err
	}
	if logWriter != nil && verbosity >= 1 {
		fmt.Fprintf(logWriter, "HTTP request: %s %s\n", req.Method, req.URL.String())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Account{}, fmt.Errorf("read response: %w", err)
	}
	if logWriter != nil && verbosity >= 1 {
		fmt.Fprintf(logWriter, "HTTP response: %s\n", resp.Status)
	}
	if logWriter != nil && verbosity >= 3 {
		fmt.Fprintf(logWriter, "HTTP body: %s\n", strings.TrimSpace(string(respBody)))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		trimmed := strings.TrimSpace(string(respBody))
		if len(trimmed) > 1024 {
			trimmed = trimmed[:1024]
		}
		return Account{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, trimmed)
	}

	var raw rawAccount
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Account{}, fmt.Errorf("decode response: %w", err)
	}

	account := Account{
		Username: firstNonEmpty(raw.Username, raw.User, raw.Email),
		Password: firstNonEmpty(raw.Password, raw.Pass),
	}
	account.SMTPAddress = firstNonEmpty(raw.SMTPAddress, raw.SMTPServer, joinHostPort(raw.SMTPHost, raw.SMTPPort))
	account.IMAPAddress = firstNonEmpty(raw.IMAPAddress, raw.IMAPServer, joinHostPort(raw.IMAPHost, raw.IMAPPort))
	return account, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func joinHostPort(host, port string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" || port == "" {
		return ""
	}
	return host + ":" + port
}
