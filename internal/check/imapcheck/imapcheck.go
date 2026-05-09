package imapcheck

import (
	"context"
	"crypto/tls"
	"io"
	"strings"

	"github.com/emersion/go-imap/v2/imapclient"
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
		done <- checkIMAP(addr, username, password, logWriter)
	}()
	select {
	case err := <-done:
		return Result{Err: err}
	case <-ctx.Done():
		return Result{Err: ctx.Err()}
	}
}

func checkIMAP(addr, username, password string, logWriter io.Writer) error {
	host := addr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host = addr[:i]
	}
	client, err := imapclient.DialTLS(addr, &imapclient.Options{
		TLSConfig:   &tls.Config{ServerName: host},
		DebugWriter: logWriter,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Login(username, password).Wait(); err != nil {
		return err
	}
	if err := client.Logout().Wait(); err != nil {
		return err
	}
	return nil
}
