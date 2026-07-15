// Package agentlog sets up structured logging for the running agent
// service. Running as an installed OS service means stdout goes nowhere
// anyone can see, so this writes structured JSON to a daily-rotating file
// instead - that log file is the only way to actually diagnose "it says
// it's running but isn't talking to the backend" after the fact.
package agentlog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const namePrefix = "agent-"
const nameSuffix = ".log"

// Setup opens dir/agent-YYYY-MM-DD.log (creating dir if needed), prunes log
// files older than keepDays, and returns a logger that writes structured
// JSON there. The returned io.Closer should be closed on shutdown.
func Setup(dir string, keepDays int, level slog.Level) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	w, err := newDailyWriter(dir, keepDays)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	return logger, w, nil
}

type dailyWriter struct {
	mu       sync.Mutex
	dir      string
	keepDays int
	current  *os.File
	date     string
}

func newDailyWriter(dir string, keepDays int) (*dailyWriter, error) {
	w := &dailyWriter{dir: dir, keepDays: keepDays}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyWriter) rotate() error {
	date := time.Now().Format("2006-01-02")
	if w.current != nil && w.date == date {
		return nil
	}
	if w.current != nil {
		w.current.Close()
	}
	pruneOld(w.dir, w.keepDays)

	f, err := os.OpenFile(filepath.Join(w.dir, namePrefix+date+nameSuffix), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.current = f
	w.date = date
	return nil
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotate(); err != nil {
		return 0, err
	}
	return w.current.Write(p)
}

func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != nil {
		return w.current.Close()
	}
	return nil
}

func pruneOld(dir string, keepDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, nameSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
}
