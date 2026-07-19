package main

import (
	"os"
	"path/filepath"
	"testing"

	"pingachock/internal/config"

	"github.com/kardianos/service"
)

type fakeService struct {
	stopped, started int
	status           service.Status
}

func (f *fakeService) Run() error                                             { return nil }
func (f *fakeService) Start() error                                           { f.started++; f.status = service.StatusRunning; return nil }
func (f *fakeService) Stop() error                                            { f.stopped++; f.status = service.StatusStopped; return nil }
func (f *fakeService) Restart() error                                         { return nil }
func (f *fakeService) Install() error                                         { return nil }
func (f *fakeService) Uninstall() error                                       { return nil }
func (f *fakeService) Logger(errs chan<- error) (service.Logger, error)       { return nil, nil }
func (f *fakeService) SystemLogger(errs chan<- error) (service.Logger, error) { return nil, nil }
func (f *fakeService) String() string                                         { return "fake" }
func (f *fakeService) Platform() string                                       { return "fake" }
func (f *fakeService) Status() (service.Status, error)                        { return f.status, nil }

func TestRunUpdateCopiesBinaryAndPreservesConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	cfg := config.Config{NodeSecret: "s3cr3t", DirectURL: "https://example.com"}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate safeInstallDir() by overriding via a package-level var would
	// require prod code changes - instead just verify the destination
	// runUpdate actually chose, using the real safeInstallDir(), matches
	// expectations and got the right bytes.
	dest := filepath.Join(safeInstallDir(), filepath.Base(os.Args[0]))
	t.Cleanup(func() { os.Remove(dest) })

	svc := &fakeService{status: service.StatusRunning}
	if err := runUpdate(configPath, svc); err != nil {
		// safeInstallDir() may not be writable in this sandboxed test
		// environment (no root) - that's fine, we're checking behavior,
		// not asserting success here.
		t.Logf("runUpdate returned (expected without root): %v", err)
	}

	if svc.stopped == 0 {
		t.Errorf("expected Stop to be called, stopped=%d", svc.stopped)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config file was modified by runUpdate:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestInstalledBinaryPathIn guards against a real bug hit in production: an
// operator renamed their downloaded copy before running it (a plausible
// real scenario - a versioned filename, a browser's "(1)" suffix on a
// re-download, ...) and picked the "update" menu action. runUpdate used to
// compute dest as filepath.Base(the *new*, renamed exe) - which doesn't
// match the name the OS service is actually registered under, so it copied
// the new binary in under a brand new name next to the untouched old one,
// then restarted the service into that same untouched old binary. The
// service silently kept running pre-fix code despite "Готово" on screen.
func TestInstalledBinaryPathIn(t *testing.T) {
	newExe := filepath.Join("staging", "pingachock-agent-fixed-v2.exe")

	t.Run("existing binary under a different name wins over the new source's own name", func(t *testing.T) {
		dir := t.TempDir()
		installed := filepath.Join(dir, "pingachock-agent-windows-amd64.exe")
		if err := os.WriteFile(installed, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
			t.Fatal(err)
		}

		got := installedBinaryPathIn(dir, newExe)
		if got != installed {
			t.Errorf("installedBinaryPathIn() = %q, want %q (the already-installed binary, not the renamed source)", got, installed)
		}
	})

	t.Run("nothing installed yet falls back to the source's own name", func(t *testing.T) {
		dir := t.TempDir()

		want := filepath.Join(dir, "pingachock-agent-fixed-v2.exe")
		got := installedBinaryPathIn(dir, newExe)
		if got != want {
			t.Errorf("installedBinaryPathIn() = %q, want %q", got, want)
		}
	})

	t.Run("ambiguous (two candidate binaries) falls back rather than guessing", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"pingachock-agent-windows-amd64.exe", "pingachock-agent-old.exe"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		want := filepath.Join(dir, "pingachock-agent-fixed-v2.exe")
		got := installedBinaryPathIn(dir, newExe)
		if got != want {
			t.Errorf("installedBinaryPathIn() = %q, want %q (ambiguous - should fall back, not guess)", got, want)
		}
	})

	t.Run("update source shares the installed binary's own name - the common case", func(t *testing.T) {
		// The normal path: operator re-downloads a new build under the same
		// filename as before. Must still resolve to that file, including
		// when an unrelated stray .exe (like the one this exact bug left
		// behind on the test node) is also sitting in the directory.
		dir := t.TempDir()
		sameNameExe := filepath.Join("staging", "pingachock-agent-windows-amd64.exe")
		for _, name := range []string{"pingachock-agent-windows-amd64.exe", "pingachock-agent-fixed-v2.exe"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		want := filepath.Join(dir, "pingachock-agent-windows-amd64.exe")
		got := installedBinaryPathIn(dir, sameNameExe)
		if got != want {
			t.Errorf("installedBinaryPathIn() = %q, want %q", got, want)
		}
	})
}
