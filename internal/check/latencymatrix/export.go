package latencymatrix

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ExportRun writes a single gzip-compressed JSON file with pair logs and run.log embedded
// in the document (no separate log files in the archive).
func ExportRun(logsDir string, res Result, outPath string) error {
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	export := res
	embedLogsFromDisk(logsDir, &export)
	for i := range export.Matrix {
		for j := range export.Matrix[i] {
			export.Matrix[i][j].LogPath = ""
		}
	}
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	_, err = gw.Write(data)
	return err
}

func embedLogsFromDisk(logsDir string, res *Result) {
	if logsDir == "" {
		return
	}
	runLog := filepath.Join(logsDir, "run.log")
	if b, err := os.ReadFile(runLog); err == nil {
		res.RunLogs = ParseLogBytes(b)
	}
	for i := range res.Matrix {
		for j := range res.Matrix[i] {
			p := res.Matrix[i][j].LogPath
			if p == "" {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			res.Matrix[i][j].Logs = ParseLogBytes(b)
		}
	}
}

// ParseLogBytes parses pair/run log files written by logPairf (`[timestamp] message`).
func ParseLogBytes(data []byte) []LogLine {
	s := string(data)
	var out []LogLine
	for _, line := range strings.Split(s, "\n") {
		ll := ParseLogLine(line)
		if ll.Timestamp == "" && ll.Message == "" {
			continue
		}
		out = append(out, ll)
	}
	return out
}

// ParseLogLine parses one line from a pair or run log.
func ParseLogLine(line string) LogLine {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return LogLine{}
	}
	if strings.HasPrefix(line, "[") {
		if idx := strings.Index(line, "] "); idx > 0 {
			return LogLine{Timestamp: line[1:idx], Message: line[idx+2:]}
		}
	}
	return LogLine{Message: line}
}
