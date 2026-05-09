package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"relay-ping/internal/chatmail"
	"relay-ping/internal/check/imapcheck"
	"relay-ping/internal/check/latencymatrix"
	"relay-ping/internal/check/securejoininit"
	"relay-ping/internal/check/smtpcheck"
	"relay-ping/internal/check/throughput"
)

func main() {
	var (
		domain   = flag.String("domain", "https://nine.testrun.org/", "chatmail provider domain")
		endpoint = flag.String("endpoint", "", "chatmail account endpoint (optional override)")
		testName = flag.String("test", "connectivity", "test to run: connectivity, securejoin-init, throughput, latency_matrix, test-webserver")
		servers  = flag.String("servers", "", "comma-separated servers for latency_matrix test")
		serversFile = flag.String("servers-file", "", "path to servers file for latency_matrix test (one host per line, '#' comments supported)")
		worker     = flag.Int("worker", 4, "concurrent pair workers for latency_matrix (each opens SMTP+IMAP; lower if you see no route / timeouts; capped at 16 inside the tool)")
		webserver = flag.Bool("webserver", false, "start local web UI (for latency_matrix)")
		webAddr   = flag.String("web-addr", "127.0.0.1:8787", "web UI listen address for -webserver")
		export    = flag.String("export", "", "export latency_matrix run to .json.gz (gzip JSON with embedded logs)")
		count    = flag.Int("count", 20, "message count for throughput test")
		workers  = flag.Int("workers", 8, "parallel workers for throughput test")
		smtpAddr = flag.String("smtp", "", "SMTP address host:port (optional override)")
		imapAddr = flag.String("imap", "", "IMAP address host:port (optional override)")
		logFile  = flag.String("log-file", "relay-ping.log", "file to write verbose logs, use '-' to disable file logging")
		color    = flag.String("color", "always", "color mode: auto, always, never")
		v1       = flag.Bool("v", false, "verbose logging")
		v2       = flag.Bool("vv", false, "very verbose logging")
		v3       = flag.Bool("vvv", false, "trace logging")
		timeout  = flag.Duration("timeout", 20*time.Second, "network timeout")
	)
	flag.Parse()
	verbosity := 0
	if *v1 {
		verbosity = 1
	}
	if *v2 {
		verbosity = 2
	}
	if *v3 {
		verbosity = 3
	}

	useColor, err := resolveColorMode(*color)
	if err != nil {
		log.Fatalf("invalid -color value: %v", err)
	}

	var (
		logWriter io.Writer = io.Discard
		httpLog   io.Writer
		smtpLog   io.Writer
		imapLog   io.Writer
		wireLog   io.Writer
		closeFn   = func() {}
	)
	if verbosity > 0 {
		baseWriter, closeBase, setupErr := setupLogWriter(*logFile)
		if setupErr != nil {
			log.Fatalf("setup logging: %v", setupErr)
		}
		closeFn = closeBase
		logWriter = newTimestampWriter(newLockedWriter(baseWriter))
		httpLog = newPrefixedWriter(logWriter, colorize(useColor, "36", "[HTTP] "))
		if verbosity >= 2 {
			smtpLog = newPrefixedWriter(logWriter, colorize(useColor, "35", "[SMTP] "))
			imapLog = newPrefixedWriter(logWriter, colorize(useColor, "34", "[IMAP] "))
			wireLog = newPrefixedWriter(logWriter, colorize(useColor, "33", "[WIRE] "))
		}
	}
	defer closeFn()

	resolvedEndpoint := *endpoint
	if resolvedEndpoint == "" {
		resolvedEndpoint, err = chatmail.EndpointFromDomain(*domain)
		if err != nil {
			log.Fatalf("resolve endpoint from domain: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if verbosity > 0 {
		fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== relay-ping started ==="))
		fmt.Fprintf(logWriter, "target domain: %s\n", *domain)
		fmt.Fprintf(logWriter, "endpoint: %s\n", resolvedEndpoint)
	}

	switch strings.ToLower(strings.TrimSpace(*testName)) {
	case "connectivity":
		account, err := chatmail.FetchAccount(ctx, resolvedEndpoint, httpLog, verbosity)
		if err != nil {
			log.Fatalf("fetch account: %v", err)
		}

		defaultSMTP, defaultIMAP, err := chatmail.DefaultMailAddrsFromDomain(*domain)
		if err != nil {
			log.Fatalf("resolve mail addresses from domain: %v", err)
		}
		if account.SMTPAddress == "" {
			account.SMTPAddress = defaultSMTP
		}
		if account.IMAPAddress == "" {
			account.IMAPAddress = defaultIMAP
		}

		if *smtpAddr != "" {
			account.SMTPAddress = *smtpAddr
		}
		if *imapAddr != "" {
			account.IMAPAddress = *imapAddr
		}

		if account.Username == "" || account.Password == "" {
			log.Fatal("account response did not include username/password")
		}
		if account.SMTPAddress == "" || account.IMAPAddress == "" {
			log.Fatal("missing smtp/imap address (pass -smtp and -imap flags to override)")
		}

		if verbosity > 0 {
			fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== checking protocols ==="))
		}
		smtpResult := smtpcheck.Check(ctx, account.SMTPAddress, account.Username, account.Password, smtpLog)
		imapResult := imapcheck.Check(ctx, account.IMAPAddress, account.Username, account.Password, imapLog)

		fmt.Println()
		fmt.Println(colorize(useColor, "1;37", "=== result ==="))
		fmt.Printf("test     : connectivity\n")
		fmt.Printf("domain   : %s\n", *domain)
		fmt.Printf("endpoint : %s\n", resolvedEndpoint)
		fmt.Printf("username : %s\n", account.Username)
		fmt.Printf("smtp     : %s -> %s\n", account.SMTPAddress, colorStatus(useColor, smtpResult.Status()))
		if smtpResult.Err != nil {
			fmt.Fprintf(os.Stderr, "%s %v\n", colorize(useColor, "31", "smtp error:"), smtpResult.Err)
		}
		fmt.Printf("imap     : %s -> %s\n", account.IMAPAddress, colorStatus(useColor, imapResult.Status()))
		if imapResult.Err != nil {
			fmt.Fprintf(os.Stderr, "%s %v\n", colorize(useColor, "31", "imap error:"), imapResult.Err)
		}
		if verbosity > 0 {
			fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== relay-ping finished ==="))
		}
		if smtpResult.Err != nil || imapResult.Err != nil {
			os.Exit(1)
		}
	case "securejoin-init":
		if verbosity > 0 {
			fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== securejoin init ==="))
		}
		sjResult := securejoininit.Run(ctx, resolvedEndpoint, *domain, logWriter, smtpLog, verbosity)
		fmt.Println()
		fmt.Println(colorize(useColor, "1;37", "=== result ==="))
		fmt.Printf("test         : securejoin-init\n")
		fmt.Printf("domain       : %s\n", *domain)
		fmt.Printf("endpoint     : %s\n", resolvedEndpoint)
		fmt.Printf("status       : %s\n", colorStatus(useColor, sjResult.Status()))
		if sjResult.InviterEmail != "" {
			fmt.Printf("inviter      : %s\n", sjResult.InviterEmail)
		}
		if sjResult.JoinerEmail != "" {
			fmt.Printf("joiner       : %s\n", sjResult.JoinerEmail)
		}
		if sjResult.InviteURI != "" {
			fmt.Printf("securejoin   : %s\n", sjResult.InviteURI)
		}
		if verbosity > 0 {
			fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== relay-ping finished ==="))
		}
		if sjResult.Err != nil {
			fmt.Fprintf(os.Stderr, "%s %v\n", colorize(useColor, "31", "securejoin error:"), sjResult.Err)
			os.Exit(1)
		}
	case "throughput":
		if verbosity > 0 {
			fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== throughput ==="))
		}
		loaderEnabled := verbosity < 2
		var loader *cliLoader
		if loaderEnabled {
			loader = newCLILoader(*count)
			loader.start()
		}
		tpResult := throughput.Run(
			ctx,
			resolvedEndpoint,
			*domain,
			*count,
			*workers,
			verbosity,
			logWriter,
			wireLog,
			func(p throughput.Progress) {
				done := p.Sent
				if p.Phase == "verify-delivery" || p.Phase == "done" {
					done = p.Delivered
				}
				if loaderEnabled {
					loader.update(p.Phase, p.Sent, done, p.Delivered, p.Failed)
				}
			},
		)
		if loaderEnabled {
			loader.stop()
		}
		model := throughputResultModel{
			useColor:         useColor,
			domain:           *domain,
			endpoint:         resolvedEndpoint,
			workers:          *workers,
			result:           tpResult,
			secureJoinStatus: colorStatus(useColor, tpResult.SecureJoinStatus),
		}
		fmt.Println(model.View())
		if verbosity > 0 {
			fmt.Fprintln(logWriter, colorize(useColor, "1;37", "=== relay-ping finished ==="))
		}
		if tpResult.Err != nil {
			fmt.Fprintf(os.Stderr, "%s %v\n", colorize(useColor, "31", "throughput error:"), tpResult.Err)
			os.Exit(1)
		}
	case "test-webserver":
		startStandaloneWebServer(*webAddr)
	case "latency_matrix", "latancy_matrix":
		serverList, err := resolveServersInput(*servers, *serversFile)
		if err != nil {
			log.Fatalf("resolve servers input: %v", err)
		}
		if len(serverList) < 2 {
			log.Fatal("latency_matrix requires at least 2 servers (use -servers or -servers-file)")
		}
		var wsSrv *matrixWebServer
		if *webserver {
			var url string
			wsSrv, url, err = startMatrixWebServer(*webAddr)
			if err != nil {
				log.Fatalf("start webserver: %v", err)
			}
			defer func() {
				_ = wsSrv.stop(context.Background())
			}()
			fmt.Printf("web ui: %s\n", url)
			openBrowser(url)
		}
		// Status lines (account creation, etc.) must use stderr so they do not scroll
		// stdout and break in-place progress updates.
		statusWriter := io.Writer(os.Stderr)
		if wsSrv != nil {
			statusWriter = io.Discard
		}
		res, err := latencymatrix.Run(ctx, serverList, *worker, verbosity, logWriter, wireLog, statusWriter, func(p latencymatrix.Progress) {
			if wsSrv != nil {
				wsSrv.publishProgress(p)
			}
			if wsSrv == nil {
				// Account provisioning logs only on stderr (full lines). Do not paint stdout
				// with \r here: stderr would append on the same terminal row and mash lines together.
				if p.CurrentFrom != "" && p.CurrentTo != "" {
					line := renderLatencyProgress(useColor, p)
					fmt.Print("\r\033[2K")
					fmt.Print(line)
				}
			}
		})
		if err != nil {
			log.Fatalf("latency_matrix failed: %v", err)
		}
		if wsSrv != nil {
			wsSrv.publishDone(res)
		}
		if wsSrv == nil {
			fmt.Println()
			fmt.Println(colorize(useColor, "1;37", "=== Latency matrix complete ==="))
			fmt.Print(summarizeLatencyMatrixCLI(useColor, res))
		}
		if wsSrv == nil && res.LogsDir != "" {
			fmt.Printf("logs: %s\n", res.LogsDir)
		}
		if *export != "" && res.LogsDir != "" {
			if err := latencymatrix.ExportRun(res.LogsDir, res, *export); err != nil {
				log.Fatalf("export failed: %v", err)
			}
			fmt.Printf("exported: %s\n", *export)
		}
		// Matrix Run has fully returned; publishDone already sent. Wait so browsers
		// process the final WS frame before defer closes connections and exits.
		if wsSrv != nil {
			time.Sleep(800 * time.Millisecond)
		}
	default:
		log.Fatalf("unknown -test value %q (expected connectivity, securejoin-init, throughput, latency_matrix, or test-webserver)", *testName)
	}
}

type cliLoader struct {
	total     int
	phase     atomic.Value
	sent      atomic.Int64
	done      atomic.Int64
	delivered atomic.Int64
	failed    atomic.Int64
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func newCLILoader(total int) *cliLoader {
	l := &cliLoader{
		total:  total,
		stopCh: make(chan struct{}),
	}
	l.phase.Store("starting")
	return l
}

func (l *cliLoader) update(phase string, sent, done, delivered, failed int) {
	l.phase.Store(phase)
	l.sent.Store(int64(sent))
	l.done.Store(int64(done))
	l.delivered.Store(int64(delivered))
	l.failed.Store(int64(failed))
}

func (l *cliLoader) start() {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		spinner := []rune{'|', '/', '-', '\\'}
		idx := 0
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		lastPhase := ""
		lastSent := int64(-1)
		lastDone := int64(-1)
		lastDelivered := int64(-1)
		lastFailed := int64(-1)
		for {
			select {
			case <-l.stopCh:
				l.render(' ', true)
				fmt.Println()
				return
			case <-ticker.C:
				phase, _ := l.phase.Load().(string)
				sent := l.sent.Load()
				done := l.done.Load()
				delivered := l.delivered.Load()
				failed := l.failed.Load()
				changed := phase != lastPhase || sent != lastSent || done != lastDone || delivered != lastDelivered || failed != lastFailed
				if changed {
					l.render(spinner[idx%len(spinner)], false)
					lastPhase, lastSent, lastDone, lastDelivered, lastFailed = phase, sent, done, delivered, failed
				}
				idx++
			}
		}
	}()
}

func (l *cliLoader) stop() {
	close(l.stopCh)
	l.wg.Wait()
}

func (l *cliLoader) render(spin rune, final bool) {
	done := int(l.done.Load())
	if done > l.total {
		done = l.total
	}
	sent := l.sent.Load()
	if sent > int64(l.total) {
		sent = int64(l.total)
	}
	phase, _ := l.phase.Load().(string)
	pct := 0.0
	if l.total > 0 {
		pct = (float64(done) * 100.0) / float64(l.total)
	}
	prefix := string(spin)
	if final {
		prefix = "✓"
	}
	fmt.Printf("\r%s phase=%s sent=%d/%d progress=%d/%d (%.1f%%) delivered=%d failed=%d",
		prefix, phase, sent, l.total, done, l.total, pct, l.delivered.Load(), l.failed.Load())
}

type throughputResultModel struct {
	useColor         bool
	domain           string
	endpoint         string
	workers          int
	result           throughput.Result
	secureJoinStatus string
}

func (m throughputResultModel) View() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(colorize(m.useColor, "1;37", "=== Throughput Result ==="))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("target      : %s\n", m.domain))
	b.WriteString(fmt.Sprintf("endpoint    : %s\n", m.endpoint))
	b.WriteString(fmt.Sprintf("workers     : %d\n", m.workers))
	b.WriteString(fmt.Sprintf("count       : %d\n\n", m.result.Total))
	b.WriteString(fmt.Sprintf("securejoin  : %s\n", m.secureJoinStatus))
	b.WriteString(fmt.Sprintf("inviter     : %s\n", m.result.InviterEmail))
	b.WriteString(fmt.Sprintf("joiner      : %s\n", m.result.JoinerEmail))
	b.WriteString(fmt.Sprintf("sender      : %s\n", m.result.SenderEmail))
	b.WriteString(fmt.Sprintf("receiver    : %s\n\n", m.result.ReceiverEmail))
	b.WriteString("tmp artifacts:\n")
	acc1State := "created"
	acc2State := "created"
	if m.result.Acc1Reused {
		acc1State = "reused"
	}
	if m.result.Acc2Reused {
		acc2State = "reused"
	}
	b.WriteString(fmt.Sprintf("  - acc1_priv.asc           (%s, %s)\n", m.result.Acc1PrivFile, acc1State))
	b.WriteString(fmt.Sprintf("  - acc1_pub.asc            (%s, %s)\n", m.result.Acc1PubFile, acc1State))
	b.WriteString(fmt.Sprintf("  - acc2_priv.asc           (%s, %s)\n", m.result.Acc2PrivFile, acc2State))
	b.WriteString(fmt.Sprintf("  - acc2_pub.asc            (%s, %s)\n", m.result.Acc2PubFile, acc2State))
	b.WriteString(fmt.Sprintf("  - acc1_acc2_raw_packet.asc (%s)\n\n", m.result.RawPacketFile))
	b.WriteString(fmt.Sprintf("delivery    : %d/%d\n", m.result.Delivered, m.result.Total))
	b.WriteString(fmt.Sprintf("accepted    : %d sent to server\n", m.result.SentAccepted))
	b.WriteString(fmt.Sprintf("send-failed : %d\n", m.result.SendFailed))
	b.WriteString(fmt.Sprintf("verify-timeout: %d\n", m.result.DeliveryTimeout))
	if m.result.RateLimited {
		b.WriteString(fmt.Sprintf("rate_limited: yes (%d events)\n", m.result.RateLimitedCount))
	} else {
		b.WriteString("rate_limited: no\n")
	}
	b.WriteString(fmt.Sprintf("mps         : %.2f\n", m.result.MPS))
	b.WriteString(fmt.Sprintf("latency     : avg %.2f ms | p95 %.2f ms\n", m.result.AvgLatencyMS, m.result.P95LatencyMS))
	b.WriteString(fmt.Sprintf("errors      : %d (%.2f%%)\n", m.result.Failed, m.result.ErrorRatePct))
	b.WriteString(fmt.Sprintf("bandwidth   : out %.2f B/s | in %.2f B/s\n", m.result.BandwidthOutBps, m.result.BandwidthInBps))
	return b.String()
}

func setupLogWriter(logPath string) (io.Writer, func(), error) {
	if logPath == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	w := io.MultiWriter(os.Stdout, f)
	closeFn := func() {
		_ = f.Close()
	}
	return w, closeFn, nil
}

func resolveColorMode(mode string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		info, err := os.Stdout.Stat()
		if err != nil {
			return false, nil
		}
		return (info.Mode() & os.ModeCharDevice) != 0, nil
	default:
		return false, fmt.Errorf("expected auto, always, or never, got %q", mode)
	}
}

func colorize(enabled bool, code, s string) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func colorStatus(enabled bool, status string) string {
	if status == "OK" {
		return colorize(enabled, "32", status)
	}
	return colorize(enabled, "31", status)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newLockedWriter(w io.Writer) io.Writer {
	return &lockedWriter{w: w}
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

type prefixedWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	buf    string
}

func newPrefixedWriter(w io.Writer, prefix string) io.Writer {
	return &prefixedWriter{w: w, prefix: prefix}
}

type timestampWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf string
}

func newTimestampWriter(w io.Writer) io.Writer {
	return &timestampWriter{w: w}
}

func (tw *timestampWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.buf += string(p)
	for {
		i := strings.IndexByte(tw.buf, '\n')
		if i < 0 {
			break
		}
		line := tw.buf[:i]
		tw.buf = tw.buf[i+1:]
		ts := time.Now().Format("15:04:05")
		if _, err := fmt.Fprintf(tw.w, "[%s] %s\n", ts, line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (pw *prefixedWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.buf += string(p)
	for {
		i := strings.IndexByte(pw.buf, '\n')
		if i < 0 {
			break
		}
		line := pw.buf[:i]
		pw.buf = pw.buf[i+1:]
		if _, err := fmt.Fprintf(pw.w, "%s%s\n", pw.prefix, line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseServersFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSuffix(line, ",")
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		k := strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

func resolveServersInput(serversArg, serversFileArg string) ([]string, error) {
	var all []string
	if strings.TrimSpace(serversArg) != "" {
		all = append(all, splitCSV(serversArg)...)
	}
	filePath := strings.TrimSpace(serversFileArg)
	if filePath == "" {
		// Optional shorthand: -servers @servers.txt
		parts := splitCSV(serversArg)
		if len(parts) == 1 && strings.HasPrefix(parts[0], "@") {
			filePath = strings.TrimPrefix(parts[0], "@")
			all = nil
		}
	}
	if filePath != "" {
		fileServers, err := parseServersFile(filePath)
		if err != nil {
			return nil, err
		}
		all = append(all, fileServers...)
	}
	return uniqueStrings(all), nil
}

// summarizeLatencyMatrixCLI prints aggregate stats and lists only failing pairs (timeout, IMAP/SMTP errors, etc.).
func summarizeLatencyMatrixCLI(useColor bool, res latencymatrix.Result) string {
	var b strings.Builder
	if len(res.Servers) == 0 {
		return "no data\n"
	}
	if res.Status != nil {
		s := res.Status
		fmt.Fprintf(&b, "servers: %d | federated: %d | avg latency (OK cells): %d ms\n",
			len(res.Servers), s.Federated, s.FedLatency)
		fmt.Fprintf(&b, "%s good (<1s) | %s ok (1-2.5s) | %s slow (>=2.5s) | %s failed\n\n",
			colorize(useColor, "32", fmt.Sprint(s.Good)),
			colorize(useColor, "33", fmt.Sprint(s.Mid)),
			colorize(useColor, "31", fmt.Sprint(s.Slow)),
			colorize(useColor, "31", fmt.Sprint(s.Bad)),
		)
	}
	n := len(res.Servers)
	var issues []string
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i >= len(res.Matrix) || j >= len(res.Matrix[i]) {
				continue
			}
			c := res.Matrix[i][j]
			st := strings.ToUpper(strings.TrimSpace(c.Status))
			switch st {
			case "", "OK", "PENDING", "TESTING":
				continue
			}
			msg := fmt.Sprintf("%s -> %s: %s", res.Servers[i], res.Servers[j], st)
			if strings.TrimSpace(c.Err) != "" {
				msg += ": " + c.Err
			}
			switch st {
			case "TIMEOUT", "SEND_ERR", "IMAP_ERR":
				msg = colorize(useColor, "31", msg)
			default:
				msg = colorize(useColor, "33", msg)
			}
			issues = append(issues, msg)
		}
	}
	if len(issues) == 0 {
		b.WriteString(colorize(useColor, "32", "no failing pairs"))
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString(colorize(useColor, "1;37", "failing pairs:\n"))
	for _, line := range issues {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Single-line status for stdout (\r redraw) during pair testing only.
func renderLatencyProgress(useColor bool, p latencymatrix.Progress) string {
	var b strings.Builder
	b.WriteString(colorize(useColor, "1;37", "latency matrix"))
	b.WriteString(fmt.Sprintf(": %s -> %s (%d/%d)", p.CurrentFrom, p.CurrentTo, p.Completed, p.Total))
	if note := strings.TrimSpace(p.Note); note != "" {
		b.WriteString(" · ")
		b.WriteString(note)
	}
	return b.String()
}
