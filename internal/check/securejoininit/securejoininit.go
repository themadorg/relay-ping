package securejoininit

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"relay-ping/internal/chatmail"
)

type Result struct {
	InviterEmail string
	JoinerEmail  string
	InviteURI    string
	Err          error
}

func (r Result) Status() string {
	if r.Err != nil {
		return "FAIL"
	}
	return "OK"
}

func Run(ctx context.Context, endpoint, domain string, logWriter, wireWriter io.Writer, verbosity int) Result {
	done := make(chan Result, 1)
	go func() {
		done <- run(endpoint, domain, logWriter, wireWriter, verbosity)
	}()
	select {
	case res := <-done:
		return res
	case <-ctx.Done():
		return Result{Err: ctx.Err()}
	}
}

func run(endpoint, domain string, logWriter, wireWriter io.Writer, verbosity int) Result {
	inviter, err := chatmail.FetchAccount(context.Background(), endpoint, logWriter, verbosity)
	if err != nil {
		return Result{Err: fmt.Errorf("create inviter account: %w", err)}
	}
	joiner, err := chatmail.FetchAccount(context.Background(), endpoint, logWriter, verbosity)
	if err != nil {
		return Result{Err: fmt.Errorf("create joiner account: %w", err)}
	}

	defaultSMTP, _, err := chatmail.DefaultMailAddrsFromDomain(domain)
	if err != nil {
		return Result{Err: fmt.Errorf("resolve default smtp: %w", err)}
	}
	if inviter.SMTPAddress == "" {
		inviter.SMTPAddress = defaultSMTP
	}
	if joiner.SMTPAddress == "" {
		joiner.SMTPAddress = defaultSMTP
	}

	pgp := crypto.PGP()
	inviterKey, err := pgp.KeyGeneration().
		AddUserId("relay-ping inviter", inviter.Username).
		New().
		GenerateKey()
	if err != nil {
		return Result{Err: fmt.Errorf("generate inviter key: %w", err)}
	}
	joinerKey, err := pgp.KeyGeneration().
		AddUserId("relay-ping joiner", joiner.Username).
		New().
		GenerateKey()
	if err != nil {
		return Result{Err: fmt.Errorf("generate joiner key: %w", err)}
	}
	fp := strings.ToUpper(inviterKey.GetFingerprint())
	inviteNumber := randomToken(24)
	authToken := randomToken(24)
	inviteURI := buildInviteURI(fp, inviteNumber, authToken, inviter.Username)

	if logWriter != nil && verbosity >= 1 {
		fmt.Fprintf(logWriter, "[SJ] inviter: %s\n", inviter.Username)
		fmt.Fprintf(logWriter, "[SJ] joiner: %s\n", joiner.Username)
		fmt.Fprintf(logWriter, "[SJ] invite-uri: %s\n", inviteURI)
	}

	raw, err := buildVCRequest(joiner.Username, inviter.Username, inviteNumber, joinerKey)
	if err != nil {
		return Result{Err: fmt.Errorf("build vc-request: %w", err)}
	}
	if err := sendVCRequest(joiner.SMTPAddress, joiner.Username, joiner.Password, inviter.Username, raw, wireWriter); err != nil {
		return Result{Err: fmt.Errorf("send vc-request: %w", err)}
	}

	return Result{
		InviterEmail: inviter.Username,
		JoinerEmail:  joiner.Username,
		InviteURI:    inviteURI,
	}
}

func buildInviteURI(fingerprint, inviteNumber, auth, inviterEmail string) string {
	return fmt.Sprintf(
		"https://i.delta.chat/#%s&i=%s&s=%s&a=%s&n=%s",
		url.QueryEscape(fingerprint),
		url.QueryEscape(inviteNumber),
		url.QueryEscape(auth),
		url.QueryEscape(inviterEmail),
		url.QueryEscape(strings.Split(inviterEmail, "@")[0]),
	)
}

func buildVCRequest(from, to, inviteNumber string, joinerKey *crypto.Key) (string, error) {
	autocryptHeader, err := buildAutocryptHeader(from, joinerKey)
	if err != nil {
		return "", err
	}
	boundary := fmt.Sprintf("securejoin-%d", time.Now().UnixNano())
	msgID := fmt.Sprintf("<relayping-%d@%s>", time.Now().UnixNano(), domainPart(from))
	raw := strings.Join([]string{
		fmt.Sprintf("From: <%s>", from),
		fmt.Sprintf("To: <%s>", to),
		fmt.Sprintf("Date: %s", time.Now().UTC().Format(time.RFC1123Z)),
		fmt.Sprintf("Message-ID: %s", msgID),
		"Subject: [...]",
		"Chat-Version: 1.0",
		"Secure-Join: vc-request",
		fmt.Sprintf("Secure-Join-Invitenumber: %s", inviteNumber),
		autocryptHeader,
		"MIME-Version: 1.0",
		fmt.Sprintf(`Content-Type: multipart/mixed; boundary="%s"`, boundary),
		"",
		fmt.Sprintf("--%s", boundary),
		"Content-Type: text/plain; charset=utf-8",
		"",
		"secure-join: vc-request",
		"",
		fmt.Sprintf("--%s--", boundary),
		"",
	}, "\r\n")
	return raw, nil
}

func domainPart(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return "localhost"
}

func randomToken(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		return strings.Repeat("a", n)
	}
	var out strings.Builder
	out.Grow(n)
	for i := 0; i < n; i++ {
		out.WriteByte(alpha[int(k[i])%len(alpha)])
	}
	return out.String()
}

func buildAutocryptHeader(addr string, key *crypto.Key) (string, error) {
	pub, err := key.ToPublic()
	if err != nil {
		return "", err
	}
	armored, err := pub.Armor()
	if err != nil {
		return "", err
	}
	keydata := armored
	keydata = strings.ReplaceAll(keydata, "-----BEGIN PGP PUBLIC KEY BLOCK-----", "")
	keydata = strings.ReplaceAll(keydata, "-----END PGP PUBLIC KEY BLOCK-----", "")
	keydata = strings.ReplaceAll(keydata, "\r", "")
	keydata = strings.ReplaceAll(keydata, "\n", "")
	keydata = strings.TrimSpace(keydata)
	return fmt.Sprintf("Autocrypt: addr=%s; prefer-encrypt=mutual; keydata=%s", addr, keydata), nil
}

func sendVCRequest(smtpAddr, username, password, rcpt, raw string, logWriter io.Writer) error {
	host, _, err := net.SplitHostPort(smtpAddr)
	if err != nil {
		host = smtpAddr
	}
	c, err := smtp.DialStartTLS(smtpAddr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	defer c.Close()
	c.DebugWriter = logWriter

	heloName := "relay-ping." + domainPart(username)
	if err := c.Hello(heloName); err != nil {
		return err
	}
	auth := sasl.NewPlainClient("", username, password)
	if err := c.Auth(auth); err != nil {
		return err
	}
	if err := c.Mail(username, nil); err != nil {
		return err
	}
	if err := c.Rcpt(rcpt, nil); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, strings.NewReader(raw)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
