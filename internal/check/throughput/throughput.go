package throughput

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"relay-ping/internal/chatmail"
	"relay-ping/internal/check/securejoininit"
)

type Result struct {
	SecureJoinStatus string
	SecureJoinInvite string
	InviterEmail     string
	JoinerEmail      string
	SenderEmail      string
	ReceiverEmail    string
	Acc1PrivFile     string
	Acc1PubFile      string
	Acc2PrivFile     string
	Acc2PubFile      string
	RawPacketFile    string
	Acc1Reused       bool
	Acc2Reused       bool
	Total            int
	SentAccepted     int
	SendFailed       int
	DeliveryTimeout  int
	RateLimitedCount int
	RateLimited      bool
	Delivered        int
	Failed           int
	MPS              float64
	AvgLatencyMS     float64
	P95LatencyMS     float64
	ErrorRatePct     float64
	BandwidthOutBps  float64
	BandwidthInBps   float64
	Err              error
}

type Progress struct {
	Phase     string
	Total     int
	Sent      int
	Delivered int
	Failed    int
}

func Run(ctx context.Context, endpoint, domain string, count, workers, verbosity int, logWriter, wireWriter io.Writer, onProgress func(Progress)) Result {
	if count <= 0 {
		count = 1
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > count {
		workers = count
	}
	start := time.Now()
	report := func(p Progress) {
		if onProgress != nil {
			onProgress(p)
		}
	}
	report(Progress{Phase: "prepare-keys", Total: count})

	keysDir := "./tmp"
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		return Result{Err: fmt.Errorf("create tmp dir: %w", err)}
	}
	acc1PrivPath := filepath.Join(keysDir, "acc1_priv.asc")
	acc1PubPath := filepath.Join(keysDir, "acc1_pub.asc")
	acc2PrivPath := filepath.Join(keysDir, "acc2_priv.asc")
	acc2PubPath := filepath.Join(keysDir, "acc2_pub.asc")
	rawPacketPath := filepath.Join(keysDir, "acc1_acc2_raw_packet.asc")

	senderPriv, _, acc1Reused, err := ensureKeyPair(acc1PrivPath, acc1PubPath, "acc1", "acc1@relay-ping.local")
	if err != nil {
		return Result{Err: err}
	}
	_, recvPub, acc2Reused, err := ensureKeyPair(acc2PrivPath, acc2PubPath, "acc2", "acc2@relay-ping.local")
	if err != nil {
		return Result{Err: err}
	}
	packet, err := buildEncryptedPacket(senderPriv, recvPub, "relay-ping throughput probe packet")
	if err != nil {
		return Result{Err: fmt.Errorf("build encrypted packet: %w", err)}
	}
	if err := os.WriteFile(rawPacketPath, []byte(packet), 0o644); err != nil {
		return Result{Err: fmt.Errorf("write encrypted packet: %w", err)}
	}
	if logWriter != nil && verbosity >= 1 {
		fmt.Fprintf(logWriter, "[TP] key material ready in %s\n", keysDir)
		fmt.Fprintf(logWriter, "[TP] encrypted raw packet written: %s (%d bytes)\n", rawPacketPath, len(packet))
	}

	report(Progress{Phase: "securejoin-precheck", Total: count})
	sj := securejoininit.Run(ctx, endpoint, domain, logWriter, wireWriter, verbosity)
	sjStatus := sj.Status()
	if sj.Err != nil {
		return Result{SecureJoinStatus: sjStatus, Err: fmt.Errorf("securejoin pre-check: %w", sj.Err)}
	}

	report(Progress{Phase: "create-accounts", Total: count})
	receiver, err := chatmail.FetchAccount(ctx, endpoint, logWriter, verbosity)
	if err != nil {
		return Result{SecureJoinStatus: sjStatus, Err: fmt.Errorf("create receiver account: %w", err)}
	}
	sender, err := chatmail.FetchAccount(ctx, endpoint, logWriter, verbosity)
	if err != nil {
		return Result{SecureJoinStatus: sjStatus, Err: fmt.Errorf("create sender account: %w", err)}
	}
	smtpDefault, imapDefault, err := chatmail.DefaultMailAddrsFromDomain(domain)
	if err != nil {
		return Result{SecureJoinStatus: sjStatus, Err: fmt.Errorf("resolve server addresses: %w", err)}
	}
	if sender.SMTPAddress == "" {
		sender.SMTPAddress = smtpDefault
	}
	if receiver.IMAPAddress == "" {
		receiver.IMAPAddress = imapDefault
	}

	report(Progress{Phase: "run-benchmark", Total: count})
	type job struct {
		index int
		msgID string
		raw   string
	}
	type acceptedMsg struct {
		msgID  string
		size   int
		sentAt time.Time
	}
	jobs := make(chan job)
	verifyJobs := make(chan acceptedMsg, count)
	verifyDone := make(chan acceptedMsg, count)
	var sentCount int64
	var deliveredCount int64
	var sendFailedCount int64
	var rateLimitedCount int64
	var sentBytes int64
	var deliveredBytes int64

	debugWriter := io.Writer(nil)
	if verbosity >= 3 {
		debugWriter = wireWriter
	}

	verifyWorkers := workers
	if verifyWorkers < 1 {
		verifyWorkers = 1
	}
	if verifyWorkers > count {
		verifyWorkers = count
	}
	var verifyWG sync.WaitGroup
	for i := 0; i < verifyWorkers; i++ {
		verifyWG.Add(1)
		go func() {
			defer verifyWG.Done()
			// Keep verify stage quiet even with -vvv to avoid huge SEARCH spam.
			c, err := dialIMAP(receiver.IMAPAddress, receiver.Username, receiver.Password, nil)
			if err != nil {
				return
			}
			defer c.Close()
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				return
			}
			for msgMeta := range verifyJobs {
				for {
					ok, err := checkDeliveredOnce(c, msgMeta.msgID)
					if err == nil && ok {
						atomic.AddInt64(&deliveredCount, 1)
						atomic.AddInt64(&deliveredBytes, int64(msgMeta.size))
						verifyDone <- msgMeta
						report(Progress{
							Phase:     "send+verify",
							Total:     count,
							Sent:      int(atomic.LoadInt64(&sentCount)),
							Delivered: int(atomic.LoadInt64(&deliveredCount)),
							Failed:    int(atomic.LoadInt64(&sendFailedCount)),
						})
						break
					}
					time.Sleep(1200 * time.Millisecond)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := range jobs {
				sendStart := time.Now()
				attempt := 0
				for {
					attempt++
					sendErr := sendRawSMTP(sender.SMTPAddress, sender.Username, sender.Password, receiver.Username, j.raw, debugWriter)
					if sendErr == nil {
						atomic.AddInt64(&sentBytes, int64(len(j.raw)))
						atomic.AddInt64(&sentCount, 1)
						verifyJobs <- acceptedMsg{
							msgID:  j.msgID,
							size:   len(j.raw),
							sentAt: sendStart,
						}
						report(Progress{
							Phase:     "send+verify",
							Total:     count,
							Sent:      int(atomic.LoadInt64(&sentCount)),
							Delivered: int(atomic.LoadInt64(&deliveredCount)),
							Failed:    int(atomic.LoadInt64(&sendFailedCount)),
						})
						break
					}
					if logWriter != nil && verbosity >= 2 {
						fmt.Fprintf(logWriter, "[TP] send retry #%d (attempt %d): %v\n", j.index, attempt, sendErr)
					}
					if isRateLimitedErr(sendErr) {
						atomic.AddInt64(&rateLimitedCount, 1)
						if logWriter != nil && verbosity >= 1 {
							fmt.Fprintf(logWriter, "[TP] rate-limited on send #%d (attempt %d)\n", j.index, attempt)
						}
					}
					atomic.AddInt64(&sendFailedCount, 1)
					report(Progress{
						Phase:     "send+verify",
						Total:     count,
						Sent:      int(atomic.LoadInt64(&sentCount)),
						Delivered: int(atomic.LoadInt64(&deliveredCount)),
						Failed:    int(atomic.LoadInt64(&sendFailedCount)),
					})
					// Keep retrying until accepted by server.
					time.Sleep(time.Duration(minInt(attempt, 20)*minInt(attempt, 20)) * 120 * time.Millisecond)
				}
			}
		}(w)
	}

	indices := make([]int, count)
	for i := range indices {
		indices[i] = i
	}
	rand.Shuffle(len(indices), func(a, b int) {
		indices[a], indices[b] = indices[b], indices[a]
	})
	for _, i := range indices {
		msgID := fmt.Sprintf("<relayping-tp-%d-%d@%s>", time.Now().UnixNano(), i, domainPart(sender.Username))
		subject := fmt.Sprintf("relay-ping throughput %d", i)
		raw := buildEncryptedRawMessage(sender.Username, receiver.Username, msgID, subject, packet)
		jobs <- job{index: i, msgID: msgID, raw: raw}
	}
	close(jobs)
	wg.Wait()
	close(verifyJobs)

	go func() {
		verifyWG.Wait()
		close(verifyDone)
	}()

	verifyStartedAt := time.Now()
	lastWaitLogAt := time.Now()
	latencies := make([]float64, 0, count)
	delivered := 0
	for msgMeta := range verifyDone {
		delivered = int(atomic.LoadInt64(&deliveredCount))
		latencies = append(latencies, float64(time.Since(msgMeta.sentAt).Milliseconds()))
		if logWriter != nil && verbosity >= 1 && time.Since(lastWaitLogAt) >= 30*time.Second {
			pendingNow := count - delivered
			fmt.Fprintf(logWriter, "[TP] still waiting for delivery: delivered=%d pending=%d waited=%s\n",
				delivered, pendingNow, time.Since(verifyStartedAt).Round(time.Second))
			lastWaitLogAt = time.Now()
		}
	}
	sentAccepted := int(atomic.LoadInt64(&sentCount))
	sendFailed := int(atomic.LoadInt64(&sendFailedCount))
	rateLimitedCountFinal := int(atomic.LoadInt64(&rateLimitedCount))
	deliveryTimeout := sentAccepted - delivered
	failed := sendFailed + deliveryTimeout

	elapsed := time.Since(start).Seconds()
	avg := avgFloat64(latencies)
	p95 := p95Float64(latencies)
	mps := 0.0
	if elapsed > 0 {
		mps = float64(delivered) / elapsed
	}
	errRate := float64(failed) * 100.0 / float64(count)
	bwOut := 0.0
	bwIn := 0.0
	if elapsed > 0 {
		bwOut = float64(atomic.LoadInt64(&sentBytes)) / elapsed
		bwIn = float64(atomic.LoadInt64(&deliveredBytes)) / elapsed
	}

	result := Result{
		SecureJoinStatus: sjStatus,
		SecureJoinInvite: sj.InviteURI,
		InviterEmail:     sj.InviterEmail,
		JoinerEmail:      sj.JoinerEmail,
		SenderEmail:      sender.Username,
		ReceiverEmail:    receiver.Username,
		Acc1PrivFile:     acc1PrivPath,
		Acc1PubFile:      acc1PubPath,
		Acc2PrivFile:     acc2PrivPath,
		Acc2PubFile:      acc2PubPath,
		RawPacketFile:    rawPacketPath,
		Acc1Reused:       acc1Reused,
		Acc2Reused:       acc2Reused,
		Total:            count,
		SentAccepted:     sentAccepted,
		SendFailed:       sendFailed,
		DeliveryTimeout:  deliveryTimeout,
		RateLimitedCount: rateLimitedCountFinal,
		RateLimited:      rateLimitedCountFinal > 0,
		Delivered:        delivered,
		Failed:           failed,
		MPS:              mps,
		AvgLatencyMS:     avg,
		P95LatencyMS:     p95,
		ErrorRatePct:     errRate,
		BandwidthOutBps:  bwOut,
		BandwidthInBps:   bwIn,
	}
	report(Progress{
		Phase:     "done",
		Total:     count,
		Sent:      delivered + failed,
		Delivered: delivered,
		Failed:    failed,
	})
	return result
}

func ensureKeyPair(privPath, pubPath, name, userID string) (*crypto.Key, *crypto.Key, bool, error) {
	if _, err := os.Stat(privPath); err == nil {
		privArmored, err := os.ReadFile(privPath)
		if err != nil {
			return nil, nil, false, fmt.Errorf("read %s: %w", privPath, err)
		}
		pubArmored, err := os.ReadFile(pubPath)
		if err != nil {
			return nil, nil, false, fmt.Errorf("read %s: %w", pubPath, err)
		}
		priv, err := crypto.NewKeyFromArmored(string(privArmored))
		if err != nil {
			return nil, nil, false, fmt.Errorf("parse private key: %w", err)
		}
		pub, err := crypto.NewKeyFromArmored(string(pubArmored))
		if err != nil {
			return nil, nil, false, fmt.Errorf("parse public key: %w", err)
		}
		return priv, pub, true, nil
	}

	pgp := crypto.PGP()
	key, err := pgp.KeyGeneration().AddUserId(name, userID).New().GenerateKey()
	if err != nil {
		return nil, nil, false, fmt.Errorf("generate %s key: %w", name, err)
	}
	pub, err := key.ToPublic()
	if err != nil {
		return nil, nil, false, fmt.Errorf("to public key: %w", err)
	}
	privArmored, err := key.Armor()
	if err != nil {
		return nil, nil, false, fmt.Errorf("armor private key: %w", err)
	}
	pubArmored, err := pub.Armor()
	if err != nil {
		return nil, nil, false, fmt.Errorf("armor public key: %w", err)
	}
	if err := os.WriteFile(privPath, []byte(privArmored), 0o600); err != nil {
		return nil, nil, false, fmt.Errorf("write %s: %w", privPath, err)
	}
	if err := os.WriteFile(pubPath, []byte(pubArmored), 0o644); err != nil {
		return nil, nil, false, fmt.Errorf("write %s: %w", pubPath, err)
	}
	return key, pub, false, nil
}

func buildEncryptedPacket(senderPriv, recvPub *crypto.Key, payload string) (string, error) {
	enc, err := crypto.PGP().Encryption().Recipient(recvPub).SigningKey(senderPriv).New()
	if err != nil {
		return "", err
	}
	msg, err := enc.Encrypt([]byte(payload))
	if err != nil {
		return "", err
	}
	return msg.Armor()
}

func buildEncryptedRawMessage(from, to, msgID, subject, armored string) string {
	boundary := fmt.Sprintf("enc-%d", time.Now().UnixNano())
	return strings.Join([]string{
		fmt.Sprintf("From: <%s>", from),
		fmt.Sprintf("To: <%s>", to),
		fmt.Sprintf("Date: %s", time.Now().UTC().Format(time.RFC1123Z)),
		fmt.Sprintf("Message-ID: %s", msgID),
		fmt.Sprintf("Subject: %s", subject),
		"MIME-Version: 1.0",
		"Chat-Version: 1.0",
		fmt.Sprintf(`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary="%s"`, boundary),
		"",
		fmt.Sprintf("--%s", boundary),
		"Content-Type: application/pgp-encrypted",
		"",
		"Version: 1",
		"",
		fmt.Sprintf("--%s", boundary),
		`Content-Type: application/octet-stream; name="encrypted.asc"`,
		`Content-Disposition: inline; filename="encrypted.asc"`,
		"",
		armored,
		"",
		fmt.Sprintf("--%s--", boundary),
		"",
	}, "\r\n")
}

func dialIMAP(addr, username, password string, debug io.Writer) (*imapclient.Client, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	c, err := imapclient.DialTLS(addr, &imapclient.Options{
		TLSConfig:   &tls.Config{ServerName: host},
		DebugWriter: debug,
	})
	if err != nil {
		return nil, err
	}
	if err := c.Login(username, password).Wait(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func waitDelivered(c *imapclient.Client, msgID string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := c.Search(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{
				{Key: "Message-ID", Value: msgID},
			},
		}, nil).Wait()
		if err != nil {
			return false, err
		}
		if len(data.AllSeqNums()) > 0 {
			return true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false, nil
}

func checkDeliveredOnce(c *imapclient.Client, msgID string) (bool, error) {
	data, err := c.Search(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{
			{Key: "Message-ID", Value: msgID},
		},
	}, nil).Wait()
	if err != nil {
		return false, err
	}
	return len(data.AllSeqNums()) > 0, nil
}

func sendRawSMTP(smtpAddr, username, password, rcpt, raw string, debug io.Writer) error {
	host, _, err := net.SplitHostPort(smtpAddr)
	if err != nil {
		host = smtpAddr
	}
	c, err := smtp.DialStartTLS(smtpAddr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	defer c.Close()
	c.DebugWriter = debug
	if err := c.Hello("relay-ping." + domainPart(username)); err != nil {
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

func domainPart(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return "localhost"
}

func avgFloat64(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func p95Float64(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := make([]float64, len(v))
	copy(cp, v)
	for i := 0; i < len(cp)-1; i++ {
		for j := i + 1; j < len(cp); j++ {
			if cp[j] < cp[i] {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	idx := int(0.95 * float64(len(cp)-1))
	return cp[idx]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isRateLimitedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate-limited") ||
		strings.Contains(s, "too much mail") ||
		strings.Contains(s, "smtp error 450") ||
		strings.Contains(s, " 4.7.1")
}
