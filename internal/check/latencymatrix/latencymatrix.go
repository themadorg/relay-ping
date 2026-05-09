package latencymatrix

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"relay-ping/internal/chatmail"
)

const deliveryTimeout = 5 * time.Second

// LogLine is one timestamped line from a pair or run log file.
type LogLine struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

type Cell struct {
	Status    string    `json:"status"`
	LatencyMS float64   `json:"latencyMS"`
	Err       string    `json:"err,omitempty"`
	LogPath   string    `json:"logPath,omitempty"`
	Logs      []LogLine `json:"logs,omitempty"`
}

// RunStatus summarizes a completed latency matrix run (matches web stats + timing).
type RunStatus struct {
	Completed  int    `json:"completed"`
	Total      int    `json:"total"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Federated  int    `json:"federated"`
	FedLatency int    `json:"fedLatency"`
	Good       int    `json:"good"`
	Mid        int    `json:"mid"`
	Slow       int    `json:"slow"`
	Bad        int    `json:"bad"`
	Failed     int    `json:"failed"`
}

type Result struct {
	Servers    []string   `json:"servers"`
	Matrix     [][]Cell   `json:"matrix"`
	LogsDir    string     `json:"logsDir,omitempty"`
	Status     *RunStatus `json:"status,omitempty"`
	RunLogs    []LogLine  `json:"runLogs,omitempty"`
	StartedAt  time.Time  `json:"-"`
	FinishedAt time.Time  `json:"-"`
}

type Progress struct {
	Result      Result `json:"result"`
	CurrentFrom string `json:"currentFrom,omitempty"`
	CurrentTo   string `json:"currentTo,omitempty"`
	Completed   int    `json:"completed"`
	Total       int    `json:"total"`
	Note        string `json:"note,omitempty"`
}

func Run(ctx context.Context, servers []string, workers, verbosity int, logWriter, wireWriter, statusWriter io.Writer, onProgress func(Progress)) (Result, error) {
	runStarted := time.Now()
	if len(servers) < 2 {
		return Result{}, fmt.Errorf("need at least 2 servers")
	}
	runLogsDir, err := createRunLogsDir()
	if err != nil {
		return Result{}, fmt.Errorf("create run logs dir: %w", err)
	}
	runMainLogPath := filepath.Join(runLogsDir, "run.log")
	runMainLog, err := os.Create(runMainLogPath)
	if err != nil {
		return Result{}, fmt.Errorf("create run log file: %w", err)
	}
	defer func() { _ = runMainLog.Close() }()
	statusOut := io.Writer(runMainLog)
	if statusWriter != nil {
		statusOut = io.MultiWriter(statusWriter, runMainLog)
	}
	// We create 2 accounts per server so we can also test intra-server (diagonal) latency.
	type serverAccounts struct {
		name    string
		sender  chatmail.Account
		receiver chatmail.Account
	}
	saList := make([]serverAccounts, 0, len(servers))
	accountDone := 0
	accountTotal := len(servers) * 2
	for _, s := range servers {
		s = strings.TrimSpace(s)
		endpoint, err := chatmail.EndpointFromDomain(s)
		if err != nil {
			statusf(statusOut, "account: failed on %s (resolve endpoint), skipping", s)
			accountDone += 2
			if onProgress != nil {
				onProgress(Progress{Completed: accountDone, Total: accountTotal, Note: fmt.Sprintf("failed on %s (resolve endpoint), skipped", s)})
			}
			continue
		}
		smtpDef, imapDef, err := chatmail.DefaultMailAddrsFromDomain(s)
		if err != nil {
			statusf(statusOut, "account: failed on %s (resolve mail addrs), skipping", s)
			accountDone += 2
			if onProgress != nil {
				onProgress(Progress{Completed: accountDone, Total: accountTotal, Note: fmt.Sprintf("failed on %s (resolve mail addrs), skipped", s)})
			}
			continue
		}
		fetchOne := func(label string) (chatmail.Account, error) {
			statusf(statusOut, "account: creating %s on %s ...", label, s)
			if onProgress != nil {
				onProgress(Progress{Completed: accountDone, Total: accountTotal, Note: fmt.Sprintf("creating %s on %s", label, s)})
			}
			acc, err := chatmail.FetchAccount(ctx, endpoint, logWriter, verbosity)
			if err != nil {
				if net.ParseIP(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")) != nil && !strings.HasPrefix(strings.ToLower(s), "http://") {
					httpEndpoint := "http://" + strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://") + "/new"
					acc, err = chatmail.FetchAccount(ctx, httpEndpoint, logWriter, verbosity)
				}
			}
			if err != nil {
				return chatmail.Account{}, err
			}
			if acc.SMTPAddress == "" {
				acc.SMTPAddress = smtpDef
			}
			if acc.IMAPAddress == "" {
				acc.IMAPAddress = imapDef
			}
			statusf(statusOut, "account: created %s on %s (%s)", label, s, acc.Username)
			accountDone++
			if onProgress != nil {
				onProgress(Progress{Completed: accountDone, Total: accountTotal, Note: fmt.Sprintf("created %s on %s", label, s)})
			}
			return acc, nil
		}
		sender, err := fetchOne("sender")
		if err != nil {
			statusf(statusOut, "account: failed creating sender on %s, skipping", s)
			accountDone++
			continue
		}
		receiver, err := fetchOne("receiver")
		if err != nil {
			statusf(statusOut, "account: failed creating receiver on %s, skipping", s)
			continue
		}
		saList = append(saList, serverAccounts{name: s, sender: sender, receiver: receiver})
	}
	if len(saList) < 1 {
		return Result{}, fmt.Errorf("need at least 1 working server after account creation, got %d", len(saList))
	}
	names := make([]string, len(saList))
	nodes := make([]chatmail.Account, len(saList))
	receiverNodes := make([]chatmail.Account, len(saList))
	for i, sa := range saList {
		names[i] = sa.name
		nodes[i] = sa.sender
		receiverNodes[i] = sa.receiver
	}

	recvKey, err := crypto.PGP().KeyGeneration().AddUserId("matrix", "matrix@relay-ping.local").New().GenerateKey()
	if err != nil {
		return Result{}, err
	}
	pub, err := recvKey.ToPublic()
	if err != nil {
		return Result{}, err
	}
	msg, err := crypto.PGP().Encryption().Recipient(pub).SigningKey(recvKey).New()
	if err != nil {
		return Result{}, err
	}
	pgpMsg, err := msg.Encrypt([]byte("relay-ping latency matrix probe"))
	if err != nil {
		return Result{}, err
	}
	packet, err := pgpMsg.Armor()
	if err != nil {
		return Result{}, err
	}

	if workers < 1 {
		workers = 1
	}
	// Hard cap: concurrency is workers goroutines (+ one job feeder), not an unbounded storm.
	// Each active worker holds outbound SMTP and later receiver IMAP traffic; too many in parallel
	// overloads local routing/conntrack and remote hosts ("no route to host", timeouts).
	const maxMatrixWorkers = 16
	if workers > maxMatrixWorkers {
		statusf(statusOut, "latency_matrix: clamping -worker %d to %d (max concurrent pair workers)", workers, maxMatrixWorkers)
		workers = maxMatrixWorkers
	}
	matrix := make([][]Cell, len(nodes))
	totalPairs := len(nodes) * len(nodes)
	completed := 0
	var mu sync.Mutex
	for i := range matrix {
		matrix[i] = make([]Cell, len(nodes))
		for j := range matrix[i] {
			matrix[i][j] = Cell{Status: "PENDING"}
		}
	}
	emit := func(from, to, note string) {
		if onProgress == nil {
			return
		}
		mu.Lock()
		snap := cloneMatrix(matrix)
		done := completed
		mu.Unlock()
		onProgress(Progress{
			Result:      Result{Servers: names, Matrix: snap, LogsDir: runLogsDir},
			CurrentFrom: from,
			CurrentTo:   to,
			Completed:   done,
			Total:       totalPairs,
			Note:        note,
		})
	}
	emit("", "", "")

	type pair struct{ i, j int }
	// Worker pool: exactly `workers` goroutines drain jobs (no per-pair goroutine explosion).
	// One IMAP session per receiver mailbox at a time — many pairs share the same j;
	// parallel logins stress servers and trigger false IMAP_ERR.
	imapLocks := make([]sync.Mutex, len(nodes))
	jobs := make(chan pair)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				i, j := p.i, p.j
				mu.Lock()
				matrix[i][j] = Cell{Status: "TESTING"}
				mu.Unlock()
				emit(names[i], names[j], "")

			msgID := fmt.Sprintf("<relayping-matrix-%d-%d-%d@%s>", time.Now().UnixNano(), i, j, domainPart(nodes[i].Username))
			raw := buildEncryptedRawMessage(nodes[i].Username, receiverNodes[j].Username, msgID, "relay-ping matrix", packet)
			start := time.Now()
			pairLog, closePairLog, pairLogPath, err := openPairLogFile(runLogsDir, names[i], names[j])
			if err != nil {
				mu.Lock()
				matrix[i][j] = Cell{Status: "IMAP_ERR", Err: "pair log create failed"}
				completed++
				mu.Unlock()
				emit(names[i], names[j], "failed opening pair log")
				continue
			}
			logPairf(pairLog, "pair=%s -> %s", names[i], names[j])
			logPairf(pairLog, "from_email=%s", nodes[i].Username)
			logPairf(pairLog, "to_email=%s", receiverNodes[j].Username)
			logPairf(pairLog, "smtp=%s", nodes[i].SMTPAddress)
			logPairf(pairLog, "imap=%s", receiverNodes[j].IMAPAddress)
			logPairf(pairLog, "message_id=%s", msgID)
			logPairf(pairLog, "timeout=%s", deliveryTimeout)
			debugWriter := pairLog
			if wireWriter != nil {
				debugWriter = io.MultiWriter(pairLog, wireWriter)
			}
			recvAcc := receiverNodes[j]

			// SMTP before IMAP: retries/backoff can take minutes; an early IMAP session
			// would sit idle and hit server timeouts, then Search fails with IMAP_ERR.
			sendErr := error(nil)
			maxAttempts := 5
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				logPairf(pairLog, "smtp_send_attempt=%d", attempt)
				sendErr = sendRawSMTP(nodes[i].SMTPAddress, nodes[i].Username, nodes[i].Password, recvAcc.Username, raw, debugWriter)
				if sendErr == nil {
					logPairf(pairLog, "smtp_send_attempt=%d status=ok", attempt)
					break
				}
				rateLimited := isRateLimited(sendErr)
				logPairf(pairLog, "smtp_send_attempt=%d status=error rate_limited=%t err=%v", attempt, rateLimited, sendErr)
				if rateLimited && maxAttempts < 20 {
					maxAttempts = 20
				}
				if rateLimited {
					sleepFor := time.Duration(attempt*attempt) * 300 * time.Millisecond
					if sleepFor > 15*time.Second {
						sleepFor = 15 * time.Second
					}
					logPairf(pairLog, "smtp_send_attempt=%d backoff=%s reason=rate_limited", attempt, sleepFor)
					time.Sleep(sleepFor)
				} else {
					sleepFor := time.Duration(attempt) * 150 * time.Millisecond
					logPairf(pairLog, "smtp_send_attempt=%d backoff=%s reason=transient", attempt, sleepFor)
					time.Sleep(sleepFor)
				}
			}
			if sendErr != nil {
				logPairf(pairLog, "final_status=SEND_ERR")
				_ = closePairLog()
				mu.Lock()
				matrix[i][j] = Cell{Status: "SEND_ERR", Err: sendErr.Error(), LogPath: pairLogPath}
				completed++
				mu.Unlock()
				emit(names[i], names[j], "")
				continue
			}

			logPairf(pairLog, "imap_phase=start")

			imapLocks[j].Lock()
			imapClient, imapErr := dialIMAP(recvAcc.IMAPAddress, recvAcc.Username, recvAcc.Password, debugWriter)
			if imapErr != nil {
				imapLocks[j].Unlock()
				logPairf(pairLog, "final_status=IMAP_ERR err=%v", imapErr)
				_ = closePairLog()
				mu.Lock()
				matrix[i][j] = Cell{Status: "IMAP_ERR", Err: imapErr.Error(), LogPath: pairLogPath}
				completed++
				mu.Unlock()
				emit(names[i], names[j], "")
				continue
			}
			if _, selErr := imapClient.Select("INBOX", nil).Wait(); selErr != nil {
				logPairf(pairLog, "final_status=IMAP_ERR err=%v", selErr)
				_ = imapClient.Close()
				imapLocks[j].Unlock()
				_ = closePairLog()
				mu.Lock()
				matrix[i][j] = Cell{Status: "IMAP_ERR", Err: selErr.Error(), LogPath: pairLogPath}
				completed++
				mu.Unlock()
				emit(names[i], names[j], "")
				continue
			}
			ok, err := waitDelivered(imapClient, msgID, deliveryTimeout, pairLog)
			if err == nil && !ok {
				logPairf(pairLog, "delivery_check=timeout reconnecting_imap=true")
				if fresh, recErr := dialIMAP(recvAcc.IMAPAddress, recvAcc.Username, recvAcc.Password, nil); recErr == nil {
					if _, selErr := fresh.Select("INBOX", nil).Wait(); selErr == nil {
						_ = imapClient.Close()
						imapClient = fresh
						ok, err = waitDelivered(imapClient, msgID, deliveryTimeout, pairLog)
					} else {
						_ = fresh.Close()
					}
				}
			}
			if err != nil {
				logPairf(pairLog, "final_status=IMAP_ERR err=%v", err)
				_ = imapClient.Close()
				imapLocks[j].Unlock()
				_ = closePairLog()
				mu.Lock()
				matrix[i][j] = Cell{Status: "IMAP_ERR", Err: err.Error(), LogPath: pairLogPath}
				completed++
				mu.Unlock()
				emit(names[i], names[j], "")
				continue
			}
			if !ok {
				logPairf(pairLog, "final_status=TIMEOUT")
				_ = imapClient.Close()
				imapLocks[j].Unlock()
				_ = closePairLog()
				mu.Lock()
				matrix[i][j] = Cell{Status: "TIMEOUT", LogPath: pairLogPath}
				completed++
				mu.Unlock()
				emit(names[i], names[j], "")
				continue
			}
			lat := float64(time.Since(start).Milliseconds())
			logPairf(pairLog, "latency_ms=%.0f", lat)
			logPairf(pairLog, "final_status=OK")
			_ = imapClient.Close()
			imapLocks[j].Unlock()
			_ = closePairLog()
			mu.Lock()
			matrix[i][j] = Cell{Status: "OK", LatencyMS: lat, LogPath: pairLogPath}
			completed++
			mu.Unlock()
			emit(names[i], names[j], "")
			}
		}()
	}
	go func() {
		defer close(jobs)
		n := len(nodes)
		pairs := make([]pair, 0, n*n)
		for i := range nodes {
			for j := range nodes {
				pairs = append(pairs, pair{i: i, j: j})
			}
		}
		rand.Shuffle(len(pairs), func(a, b int) {
			pairs[a], pairs[b] = pairs[b], pairs[a]
		})
		for _, p := range pairs {
			jobs <- p
		}
	}()
	wg.Wait()
	mu.Lock()
	final := cloneMatrix(matrix)
	mu.Unlock()

	finished := time.Now()
	st := ComputeRunStatus(names, final, runStarted, finished)
	return Result{
		Servers:    names,
		Matrix:     final,
		LogsDir:    runLogsDir,
		Status:     st,
		StartedAt:  runStarted,
		FinishedAt: finished,
	}, nil
}

func cloneMatrix(in [][]Cell) [][]Cell {
	out := make([][]Cell, len(in))
	for i := range in {
		out[i] = make([]Cell, len(in[i]))
		copy(out[i], in[i])
	}
	return out
}

// ComputeRunStatus aggregates matrix stats (aligned with web/static/app.js renderStats).
func ComputeRunStatus(servers []string, matrix [][]Cell, started, finished time.Time) *RunStatus {
	n := len(servers)
	totalPairs := n * n
	tsFmt := "2006-01-02 15:04:05.000"
	base := &RunStatus{
		Completed:  totalPairs,
		Total:      totalPairs,
		StartedAt:  started.Format(tsFmt),
		FinishedAt: finished.Format(tsFmt),
	}
	if n == 0 || len(matrix) != n {
		return base
	}
	connected := make([]bool, n)
	good, mid, slow, bad := 0, 0, 0, 0
	var latSum float64
	latCount := 0
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			c := matrix[i][j]
			st := strings.ToUpper(strings.TrimSpace(c.Status))
			switch st {
			case "OK":
				ms := c.LatencyMS
				connected[i] = true
				connected[j] = true
				latSum += ms
				latCount++
				if ms < 1000 {
					good++
				} else if ms < 2500 {
					mid++
				} else {
					slow++
				}
			case "TIMEOUT", "SEND_ERR", "IMAP_ERR":
				bad++
			default:
				if st != "" && st != "PENDING" && st != "TESTING" {
					bad++
				}
			}
		}
	}
	federated := 0
	for _, v := range connected {
		if v {
			federated++
		}
	}
	fedLat := 0
	if latCount > 0 {
		fedLat = int(math.Round(latSum / float64(latCount)))
	}
	base.Federated = federated
	base.FedLatency = fedLat
	base.Good = good
	base.Mid = mid
	base.Slow = slow
	base.Bad = bad
	base.Failed = bad
	return base
}

func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate-limited") || strings.Contains(s, "smtp error 450") || strings.Contains(s, " 4.7.1")
}

func waitDelivered(c *imapclient.Client, msgID string, timeout time.Duration, debug io.Writer) (bool, error) {
	deadline := time.Now().Add(timeout)
	polls := 0
	for time.Now().Before(deadline) {
		polls++
		data, err := c.Search(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: msgID}},
		}, nil).Wait()
		if err != nil {
			logPairf(debug, "imap_search_poll=%d status=error err=%v", polls, err)
			return false, err
		}
		if len(data.AllSeqNums()) > 0 {
			logPairf(debug, "imap_search_poll=%d status=found", polls)
			return true, nil
		}
		logPairf(debug, "imap_search_poll=%d status=not_found", polls)
		time.Sleep(300 * time.Millisecond)
	}
	logPairf(debug, "imap_search_status=timeout total_polls=%d", polls)
	return false, nil
}

func dialIMAP(addr, username, password string, debug io.Writer) (*imapclient.Client, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	tlsCfg := &tls.Config{ServerName: host}
	if net.ParseIP(host) != nil {
		tlsCfg.InsecureSkipVerify = true
	}
	c, err := imapclient.DialTLS(addr, &imapclient.Options{TLSConfig: tlsCfg, DebugWriter: debug})
	if err != nil {
		return nil, err
	}
	if err := c.Login(username, password).Wait(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func sendRawSMTP(smtpAddr, username, password, rcpt, raw string, debug io.Writer) error {
	host, _, err := net.SplitHostPort(smtpAddr)
	if err != nil {
		host = smtpAddr
	}
	tlsCfg := &tls.Config{ServerName: host}
	if net.ParseIP(host) != nil {
		tlsCfg.InsecureSkipVerify = true
	}
	c, err := smtp.DialStartTLS(smtpAddr, tlsCfg)
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

func domainPart(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return "localhost"
}

func createRunLogsDir() (string, error) {
	runID := time.Now().Format("20060102-150405") + fmt.Sprintf("-%d", time.Now().UnixNano()%1_000_000)
	path := filepath.Join("tmp", "latency-matrix-"+runID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func safeName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_", "\t", "_", "@", "_")
	s = replacer.Replace(s)
	if s == "" {
		return "unknown"
	}
	return s
}

func openPairLogFile(runDir, fromServer, toServer string) (io.Writer, func() error, string, error) {
	fromDir := filepath.Join(runDir, safeName(fromServer))
	if err := os.MkdirAll(fromDir, 0o755); err != nil {
		return nil, nil, "", err
	}
	path := filepath.Join(fromDir, safeName(toServer)+".log")
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, "", err
	}
	return f, f.Close, path, nil
}

func logPairf(w io.Writer, format string, a ...any) {
	if w == nil {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	_, _ = fmt.Fprintf(w, "[%s] %s\n", ts, fmt.Sprintf(format, a...))
}

func statusf(w io.Writer, format string, a ...any) {
	if w == nil {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	_, _ = fmt.Fprintf(w, "[%s] %s\n", ts, fmt.Sprintf(format, a...))
}
