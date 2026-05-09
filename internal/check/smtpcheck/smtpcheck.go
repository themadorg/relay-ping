package smtpcheck

import (
	"context"
	"crypto/tls"
	"io"
	"strings"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type Result struct {
	Err error
}

func (r Result) Status() string {
	if r.Err != nil {
		return "FAIL"
	}
	return "OK"
}

func Check(ctx context.Context, addr, username, password string, logWriter io.Writer) Result {
	done := make(chan error, 1)
	go func() {
		done <- checkSMTP(addr, username, password, logWriter)
	}()
	select {
	case err := <-done:
		return Result{Err: err}
	case <-ctx.Done():
		return Result{Err: ctx.Err()}
	}
}

func checkSMTP(addr, username, password string, logWriter io.Writer) error {
	host := addr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host = addr[:i]
	}
	client, err := smtp.DialStartTLS(addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	defer client.Close()
	client.DebugWriter = logWriter

	auth := sasl.NewPlainClient("", username, password)
	if err := client.Auth(auth); err != nil {
		return err
	}
	if err := client.Quit(); err != nil {
		return err
	}
	return nil
}
