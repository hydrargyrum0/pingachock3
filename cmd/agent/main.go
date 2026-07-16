// The agent doubles as its own installer and setup wizard, aimed at being
// runnable by just right-clicking the exe -> "Run as administrator" on a
// machine that has nothing else on it:
//   - no arguments, first run (no config yet) -> `setup`: interactively (or
//     via flags, so a shortcut can bake in the answers for unattended
//     rollout) picks a network interface, fills in the rest of agent.json,
//     then installs and starts the native OS service in one go.
//   - no arguments, already configured -> `menu`: an interactive menu to
//     check status, reconfigure, start/stop/restart, or fully remove -
//     re-running the wizard unprompted once it's already working would be
//     surprising, so a repeat double-click lands here instead.
//   - `configure` does just the config part, without installing.
//   - `install`/`start`/`stop`/`uninstall` control the native OS service
//     (systemd/launchd/Windows Service via kardianos/service) individually.
//   - `run` runs in the foreground - also what the installed service execs.
//
// Every action pauses for a keypress before exiting (except `run` and
// `menu`, which have their own blocking loops) so a double-clicked console
// window doesn't just flash and vanish before the operator can read what
// happened.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"

	"pingachock/internal/agentlog"
	"pingachock/internal/agentstate"
	"pingachock/internal/checks"
	"pingachock/internal/config"
	"pingachock/internal/netiface"
	"pingachock/internal/poller"
	"pingachock/internal/transport"
)

const agentVersion = "0.1.0"

// stdin is shared process-wide for every interactive prompt. Deliberately a
// single instance: bufio.Reader buffers ahead, so creating a second one
// wrapping the same os.Stdin (e.g. one per function) silently drops
// whatever the first one already buffered - every prompt after that point
// reads the wrong line. Fine to share since this CLI never reads stdin
// concurrently.
var stdin = bufio.NewReader(os.Stdin)

type program struct {
	log        *slog.Logger
	configPath string
	cancel     context.CancelFunc
	done       chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.run(ctx)
	return nil
}

func (p *program) run(ctx context.Context) {
	defer close(p.done)

	// Running as an installed service means stdout goes nowhere anyone can
	// see - switch to a structured JSON file log so "it says it's running
	// but won't connect" is actually diagnosable after the fact. Falls back
	// to the console logger (still useless for a real service, but better
	// than crashing) if the log directory can't be created.
	baseDir := filepath.Dir(p.configPath)
	logsDir := filepath.Join(baseDir, "logs")
	if fileLog, closer, err := agentlog.Setup(logsDir, 14, slog.LevelDebug); err != nil {
		p.log.Error("failed to set up file logging, continuing with console only", "logs_dir", logsDir, "error", err)
	} else {
		p.log = fileLog
		defer closer.Close()
	}

	cfg, err := config.Load(p.configPath)
	if err != nil {
		p.log.Error("load config", "path", p.configPath, "error", err)
		return
	}

	netCfg := buildNetConfig(cfg)

	// cfg.LocalAddr is the interface picked for running checks (see
	// buildNetConfig above) - backend communication is deliberately NOT
	// bound to it and always goes over the OS's default route instead. A
	// bound path can be unreachable (blocked route, ISP quirk) even when
	// the default route works fine - hit this for real in production, see
	// docs/ARCHITECTURE.md.
	direct := transport.NewDirect(nil, cfg.DirectURL, cfg.NodeSecret, 30*time.Second, p.log)

	var tr transport.Transport = direct
	if cfg.FrontDomain != "" && cfg.FrontRealHost != "" {
		fronted := transport.NewFronted(nil, cfg.FrontDomain, cfg.FrontRealHost, cfg.NodeSecret, 30*time.Second, p.log)
		tr = transport.NewFailover(direct, fronted, p.log)
		p.log.Info("fronted transport configured as fallback", "front_domain", cfg.FrontDomain, "front_real_host", cfg.FrontRealHost)
	}

	pl := &poller.Poller{
		Transport:     tr,
		Interval:      time.Duration(cfg.PollIntervalSeconds) * time.Second,
		AgentVersion:  agentVersion,
		MaxConcurrent: cfg.MaxConcurrentChecks,
		NetConfig:     netCfg,
		Log:           p.log,
		StatePath:     agentstate.Path(baseDir),
	}

	p.log.Info("agent starting",
		"agent_version", agentVersion, "direct_url", cfg.DirectURL, "poll_interval", pl.Interval, "node_id", cfg.NodeID,
		"interface", cfg.InterfaceName, "local_addr", cfg.LocalAddr, "dns_servers", cfg.DNSServers,
		"fronted_configured", cfg.FrontDomain != "" && cfg.FrontRealHost != "", "logs_dir", logsDir)
	pl.Run(ctx)
	p.log.Info("agent stopped")
}

// buildNetConfig turns the interface/DNS settings picked at setup time into
// the resolver+dialer checks actually use. See internal/checks.NetConfig.
func buildNetConfig(cfg config.Config) checks.NetConfig {
	var netCfg checks.NetConfig
	if cfg.LocalAddr != "" {
		netCfg.LocalAddr = net.ParseIP(cfg.LocalAddr)
	}
	if len(cfg.DNSServers) > 0 {
		servers := cfg.DNSServers
		localIP := netCfg.LocalAddr
		netCfg.Resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				if localIP != nil {
					if strings.HasPrefix(network, "tcp") {
						d.LocalAddr = &net.TCPAddr{IP: localIP}
					} else {
						d.LocalAddr = &net.UDPAddr{IP: localIP}
					}
				}
				// First configured server; if it's unreachable the lookup
				// just fails for that check the way any DNS outage would.
				return d.DialContext(ctx, network, net.JoinHostPort(servers[0], "53"))
			},
		}
	}
	return netCfg
}

func (p *program) Stop(s service.Service) error {
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
	return nil
}

// setupFlags are the command-line overrides that let a shortcut bake in
// every answer up front, so "Run as administrator" on that shortcut needs
// zero typing - useful for rolling the same setup out to several machines.
type setupFlags struct {
	nodeSecret    string
	directURL     string
	iface         string // interface name, or "auto"
	frontDomain   string
	frontRealHost string
}

func main() {
	requireElevated()

	args := os.Args[1:]
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet("pingachock-agent", flag.ExitOnError)
	configPath := fs.String("config", "agent.json", "path to agent config file")
	nodeSecret := fs.String("node-secret", "", "node secret - skips the interactive prompt")
	directURL := fs.String("direct-url", "", "backend URL WITH scheme, e.g. https://backend.example.com:30031 - skips the interactive prompt")
	ifaceFlag := fs.String("interface", "", `network interface name, or "auto" for the first one that's up - skips the interactive prompt`)
	frontDomain := fs.String("front-domain", "", "fronted transport (optional): disguise SNI domain, bare hostname, NO https://, e.g. google.com")
	frontRealHost := fs.String("front-real-host", "", "fronted transport (optional): real Cloud Run hostname, bare hostname, NO https://, e.g. xxxxx-uc.a.run.app")
	fs.Parse(args)

	exePath, exeErr := os.Executable()
	resolvedConfig := *configPath
	if !filepath.IsAbs(resolvedConfig) && exeErr == nil {
		resolvedConfig = filepath.Join(filepath.Dir(exePath), resolvedConfig)
	}

	// This copy of the exe might be sitting somewhere with no config next
	// to it (a stray Desktop/Downloads shortcut, a copy someone grabbed
	// off a USB stick, ...) while the node was already set up from a
	// different copy and relocated to the canonical system location below.
	// Only kick in for the default relative config path - an explicit
	// -config always means exactly that path, no fallback. Without this,
	// double-clicking the "wrong" copy would silently offer to set up a
	// second, disconnected install instead of managing the real one.
	if *configPath == "agent.json" && !isConfigured(resolvedConfig) {
		if canonical := filepath.Join(safeInstallDir(), filepath.Base(*configPath)); canonical != resolvedConfig && isConfigured(canonical) {
			fmt.Printf("Рядом с этим файлом конфига нет, но узел уже настроен и работает (конфиг: %s) - использую его.\n\n", canonical)
			resolvedConfig = canonical
		}
	}

	sf := setupFlags{
		nodeSecret: *nodeSecret, directURL: *directURL, iface: *ifaceFlag,
		frontDomain: *frontDomain, frontRealHost: *frontRealHost,
	}

	// Bare double-click / "Run as administrator" with no arguments: first
	// time, run the guided setup in one shot. Once a config already exists
	// (i.e. it's already been set up and is presumably running quietly as
	// a service), the same bare double-click instead opens a menu to
	// check/reconfigure/control it - re-running the full wizard unprompted
	// would be surprising once it's already working. Anything invoking
	// this binary with explicit args (including the OS service manager,
	// which always passes "run") is unaffected either way.
	if action == "" {
		if isConfigured(resolvedConfig) {
			action = "menu"
		} else {
			action = "setup"
		}
	}

	// Get off any directory that isn't actually usable for a system
	// service before touching it further: macOS TCC gates
	// Documents/Desktop/Downloads even for root, Windows Defender's
	// Controlled Folder Access protects the same set by default for
	// SYSTEM-run processes, and beyond those known cases, relocateIfRisky
	// also actively tests whether the folder is writable at all (locked-
	// down folder, read-only mount, network share, ...) so this works from
	// literally any starting directory, not just the ones bitten by name
	// during rollout - see docs/ARCHITECTURE.md. Re-execs from the new
	// location and never returns if it relocates; reports the problem and
	// exits if the directory is unusable and relocation itself fails.
	if exeErr == nil && (action == "setup" || action == "install" || action == "configure" || action == "menu") {
		if newExe, newConfig, relocated := relocateIfRisky(exePath, resolvedConfig); relocated {
			fmt.Printf("Папка %s не подходит для работы сервиса - переношу в %s...\n\n", filepath.Dir(exePath), filepath.Dir(newExe))
			reExecFrom(newExe, buildReExecArgs(action, newConfig, sf))
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	prg := &program{log: logger, configPath: resolvedConfig}
	svc, err := service.New(prg, &service.Config{
		Name:        "pingachock-agent",
		DisplayName: "Pingachock Node Agent",
		Description: "Runs network availability checks dispatched by the pingachock backend.",
		Arguments:   []string{"run", "-config", resolvedConfig},
	})
	if err != nil {
		logger.Error("init service", "error", err)
		os.Exit(1)
	}

	switch action {
	case "setup":
		runSetup(resolvedConfig, sf, svc)
		pauseBeforeExit()
	case "menu":
		runMenu(resolvedConfig, sf, svc)
	case "configure":
		if err := runConfigure(resolvedConfig, sf); err != nil {
			fmt.Fprintln(os.Stderr, "configure failed:", err)
		}
		pauseBeforeExit()
	case "install", "uninstall", "start", "stop", "restart":
		runControlAction(svc, action)
		pauseBeforeExit()
	case "run":
		if err := svc.Run(); err != nil {
			logger.Error("run", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: %s [setup|configure|install|uninstall|start|stop|restart|run] [flags]\n", action, filepath.Base(os.Args[0]))
		pauseBeforeExit()
		os.Exit(1)
	}
}

// runSetup resolves the config (prompting for whatever wasn't given via
// flags) then installs and starts the OS service - the one-shot flow behind
// a bare double-click.
func runSetup(configPath string, f setupFlags, svc service.Service) {
	clearScreen()
	cfg, err := resolveConfig(configPath, f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup failed:", err)
		return
	}
	if err := config.Save(configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "setup failed:", err)
		return
	}
	fmt.Printf("Конфиг сохранён: %s\n\n", configPath)

	fmt.Println("Устанавливаю сервис...")
	if err := service.Control(svc, "install"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already") {
			fmt.Println("install: сервис уже установлен, пропускаю")
		} else {
			fmt.Printf("install: %v\n", err)
			printIfPermissionError(err)
			return
		}
	} else {
		fmt.Println("install: ok")
	}

	fmt.Println("Запускаю сервис...")
	if err := startService(svc); err != nil {
		fmt.Printf("start: %v\n", err)
		printIfPermissionError(err)
		return
	}
	fmt.Println("start: ok")
	fmt.Println()
	fmt.Println("Готово - узел настроен и запущен как системный сервис.")
	fmt.Printf("Подробные логи - в папке %s (по одному файлу на день).\n", filepath.Join(filepath.Dir(configPath), "logs"))
	fmt.Printf("Запусти этот файл ещё раз в любой момент, чтобы увидеть статус и меню управления.\n")
}

func isConfigured(configPath string) bool {
	cfg, err := config.Read(configPath)
	if err != nil {
		return false
	}
	return cfg.NodeSecret != "" && cfg.DirectURL != ""
}

func runControlAction(svc service.Service, action string) {
	switch action {
	case "start":
		if err := startService(svc); err != nil {
			fmt.Printf("start: %v\n", err)
			printIfPermissionError(err)
			return
		}
		fmt.Println("start: ok")
	case "restart":
		_ = service.Control(svc, "stop") // best-effort - fine if it wasn't running
		if err := startService(svc); err != nil {
			fmt.Printf("restart: %v\n", err)
			printIfPermissionError(err)
			return
		}
		fmt.Println("restart: ok")
	default:
		if err := service.Control(svc, action); err != nil {
			fmt.Printf("%s: %v\n", action, err)
			printIfPermissionError(err)
			return
		}
		fmt.Printf("%s: ok\n", action)
	}
}

// startService wraps service.Control(svc, "start") with a macOS-specific
// recovery path: kardianos drives launchd via the old `launchctl load`
// API, which we hit failing with a bare, undiagnosable
// "Load failed: 5: Input/output error" after install/uninstall cycles -
// bootout-ing any stale registration and bootstrap-ing fresh is what
// actually fixed it when debugged by hand, so do that automatically before
// giving up.
func startService(svc service.Service) error {
	err := service.Control(svc, "start")
	if err == nil {
		return nil
	}
	if runtime.GOOS != "darwin" {
		return err
	}
	fmt.Println("start через launchctl load не сработал, пробую launchctl bootstrap напрямую...")
	if rerr := darwinBootstrapRecover(svc); rerr != nil {
		fmt.Println("bootstrap тоже не помог:", rerr)
		return err
	}
	fmt.Println("сработало через bootstrap.")
	return nil
}

func darwinBootstrapRecover(svc service.Service) error {
	const plistPath = "/Library/LaunchDaemons/pingachock-agent.plist"
	exec.Command("launchctl", "bootout", "system/pingachock-agent").Run() // best-effort, fine if not currently loaded
	out, err := exec.Command("launchctl", "bootstrap", "system", plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	time.Sleep(time.Second)
	if st, err := svc.Status(); err != nil || st != service.StatusRunning {
		return fmt.Errorf("service still not running after bootstrap")
	}
	return nil
}

// formatRelative returns a complete phrase ("никогда", "12 сек назад") -
// callers shouldn't append their own "назад", the zero-time case has none.
func formatRelative(t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d сек назад", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
	}
}

// requireElevated refuses to run at all without admin/root rights - every
// action this binary has (installing/controlling a system service, writing
// to the standard system install location) needs them anyway, and running
// unprivileged just to fail confusingly partway through (or worse, silently
// succeed against a directory that later turns out unusable to the actual
// service) is worse than refusing up front with a clear fix.
func requireElevated() {
	if isElevated() {
		return
	}
	fmt.Println("Этот файл нужно запускать с правами администратора.")
	if runtime.GOOS == "windows" {
		fmt.Println(`Правой кнопкой мыши по файлу -> "Запуск от имени администратора".`)
	} else {
		fmt.Printf("Запусти через sudo: sudo ./%s\n", filepath.Base(os.Args[0]))
	}
	pauseBeforeExit()
	os.Exit(1)
}

// clearScreen resets the terminal before each interactive screen - cmd.exe
// doesn't reliably honor the ANSI clear sequence, so shell out to `cls`
// there instead of relying on an escape code.
func clearScreen() {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "cls")
	} else {
		cmd = exec.Command("clear")
	}
	cmd.Stdout = os.Stdout
	cmd.Run()
}

func statusString(svc service.Service) string {
	st, err := svc.Status()
	if err != nil {
		return fmt.Sprintf("не установлен как сервис (%v)", err)
	}
	switch st {
	case service.StatusRunning:
		return "запущен"
	case service.StatusStopped:
		return "остановлен"
	default:
		return "неизвестно"
	}
}

// runMenu is what a repeat double-click shows once the node is already
// configured: check status, reconfigure, control the service, or remove it
// entirely - without having to remember any command-line flags.
func runMenu(configPath string, f setupFlags, svc service.Service) {
	reader := stdin
	baseDir := filepath.Dir(configPath)
	statePath := agentstate.Path(baseDir)
	logsDir := filepath.Join(baseDir, "logs")

	for {
		clearScreen()
		cfg, _ := config.Read(configPath)
		st, stErr := agentstate.Load(statePath)
		running := false
		if s, err := svc.Status(); err == nil && s == service.StatusRunning {
			running = true
		}

		fmt.Println()
		fmt.Println("=== pingachock-agent ===")
		fmt.Printf("Статус сервиса: %s\n", statusString(svc))
		fmt.Printf("Лог-файл:       %s\n", filepath.Join(logsDir, "agent-"+time.Now().Format("2006-01-02")+".log"))
		fmt.Println()

		if stErr != nil {
			fmt.Println("Сводка: нет данных - сервис ещё ни разу не выходил на связь с бекендом.")
		} else {
			if !running {
				fmt.Println("(сервис сейчас не запущен - сводка ниже от последнего раза, когда он работал)")
			}
			fmt.Printf("Транспорт:        %s\n", st.Transport)
			if st.LastPollOK {
				fmt.Printf("Последний опрос:  %s, успешно (заданий получено: %d)\n", formatRelative(st.LastPollAt), st.LastJobsCount)
			} else {
				fmt.Printf("Последний опрос:  %s, ОШИБКА: %s\n", formatRelative(st.LastPollAt), st.LastPollError)
			}
			if st.ConsecutivePollFails > 0 {
				fmt.Printf("Подряд ошибок:    %d - похоже, сервис не может достучаться до бекенда\n", st.ConsecutivePollFails)
			}
			if !st.LastResultsAt.IsZero() {
				if st.LastResultsOK {
					fmt.Printf("Посл. отправка:   %s, успешно\n", formatRelative(st.LastResultsAt))
				} else {
					fmt.Printf("Посл. отправка:   %s, ОШИБКА: %s\n", formatRelative(st.LastResultsAt), st.LastResultsError)
				}
			}
			fmt.Printf("Работает с:       %s\n", formatRelative(st.StartedAt))
		}
		fmt.Println()

		fmt.Printf("Бекенд:         %s\n", cfg.DirectURL)
		if cfg.FrontDomain != "" {
			fmt.Printf("Резерв (Cloud Run): %s -> %s\n", cfg.FrontDomain, cfg.FrontRealHost)
		}
		fmt.Printf("Интерфейс проверок: %s (%s) - с бекендом агент общается через системный маршрут по умолчанию\n", cfg.InterfaceName, cfg.LocalAddr)
		fmt.Println()
		fmt.Println("1) Перенастроить (интерфейс/секрет/URL/резервный канал)")
		fmt.Println("2) Остановить сервис")
		fmt.Println("3) Запустить сервис")
		fmt.Println("4) Перезапустить сервис")
		fmt.Println("5) Удалить полностью (сервис + конфиг)")
		fmt.Println("0) Выход")

		switch promptString(reader, "Выбери действие: ") {
		case "1":
			newCfg, err := resolveConfig(configPath, f)
			if err != nil {
				fmt.Println("Ошибка:", err)
				continue
			}
			if err := config.Save(configPath, newCfg); err != nil {
				fmt.Println("Ошибка:", err)
				continue
			}
			fmt.Println("Конфиг сохранён.")
			if a := promptString(reader, "Перезапустить сейчас, чтобы применить? [Y/n]: "); !strings.EqualFold(a, "n") && !strings.EqualFold(a, "no") && !strings.EqualFold(a, "нет") {
				runControlAction(svc, "restart")
			}
		case "2":
			runControlAction(svc, "stop")
		case "3":
			runControlAction(svc, "start")
		case "4":
			runControlAction(svc, "restart")
		case "5":
			a := promptString(reader, "Точно удалить сервис и конфиг? Это необратимо [y/N]: ")
			if strings.EqualFold(a, "y") || strings.EqualFold(a, "yes") || strings.EqualFold(a, "д") || strings.EqualFold(a, "да") {
				_ = service.Control(svc, "stop")
				runControlAction(svc, "uninstall")
				if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
					fmt.Println("Не удалось удалить конфиг:", err)
				} else {
					fmt.Println("Конфиг удалён.")
				}
				fmt.Println("Готово. Можно закрыть окно и удалить сам .exe файл.")
				return
			}
			fmt.Println("Отменено.")
		case "0", "":
			return
		default:
			fmt.Println("Не понял выбор, попробуй ещё раз.")
		}
	}
}

func printIfPermissionError(err error) {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "denied") || strings.Contains(msg, "administrator") || strings.Contains(msg, "permission") {
		fmt.Println(`Похоже, не хватает прав. Перезапусти этот файл через ПКМ -> "Запуск от имени администратора" (Windows) или через sudo (Linux/macOS).`)
	}
}

// riskyDir reports whether dir is somewhere a system service reading files
// from it can silently fail: macOS TCC gates Documents/Desktop/Downloads
// even for root; Windows Defender's Controlled Folder Access protects the
// same set by default for SYSTEM-run processes. Hit this for real on both
// during rollout.
func riskyDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dirLower := strings.ToLower(dir)
	for _, name := range []string{"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music"} {
		risky := strings.ToLower(filepath.Join(home, name))
		if dirLower == risky || strings.HasPrefix(dirLower, risky+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// safeInstallDir is where the agent relocates itself to before installing
// as a system service, if it finds itself in a riskyDir.
func safeInstallDir() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "pingachock-agent")
	}
	return "/usr/local/pingachock-agent"
}

// canWriteDir reports whether dir is actually writable, by attempting a
// real write rather than trusting a fixed list of folder names - this is
// what lets the agent run from literally any starting directory and still
// notice a problem riskyDir doesn't know the name of (an unlisted
// Defender/AV-protected folder, a read-only mount, a network share without
// write access, etc).
func canWriteDir(dir string) bool {
	probe := filepath.Join(dir, ".pingachock-write-test")
	f, err := os.Create(probe)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}

// relocateIfRisky copies the running binary (and existing config, if any)
// out of a directory it can't safely run a service from into
// safeInstallDir(). If neither the current directory nor the standard
// system location turns out to be usable, reports why and exits rather
// than silently continuing into a less clear failure the next time the
// directory is touched (config save, log write, service install, ...).
func relocateIfRisky(exePath, configPath string) (newExe, newConfig string, relocated bool) {
	exeDir := filepath.Dir(exePath)
	if !riskyDir(exeDir) && canWriteDir(exeDir) {
		return exePath, configPath, false
	}
	dest := safeInstallDir()
	if filepath.Clean(exeDir) == filepath.Clean(dest) {
		reportDirProblem(exeDir, dest, fmt.Errorf("папка недоступна для записи"))
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		reportDirProblem(exeDir, dest, err)
	}
	newExePath := filepath.Join(dest, filepath.Base(exePath))
	if err := copyFile(exePath, newExePath, 0o755); err != nil {
		reportDirProblem(exeDir, dest, err)
	}
	newConfigPath := filepath.Join(dest, filepath.Base(configPath))
	if _, err := os.Stat(configPath); err == nil {
		_ = copyFile(configPath, newConfigPath, 0o600)
	}
	return newExePath, newConfigPath, true
}

// reportDirProblem explains that neither the current directory nor the
// standard system install location could be used, and what to do about
// it, then exits.
func reportDirProblem(exeDir, dest string, err error) {
	fmt.Println()
	fmt.Println("ПРОБЛЕМА: не получилось найти рабочую папку для узла.")
	fmt.Printf("  Текущая папка (%s) недоступна для записи.\n", exeDir)
	fmt.Printf("  Перенос в стандартную папку (%s) тоже не удался: %v\n", dest, err)
	fmt.Println()
	fmt.Println("Как исправить:")
	if runtime.GOOS == "windows" {
		fmt.Println(`  - запусти этот файл через ПКМ -> "Запуск от имени администратора";`)
	} else {
		fmt.Println("  - запусти этот файл через sudo;")
	}
	fmt.Printf("  - или вручную перенеси .exe в папку, где точно есть право на запись (например, %s), и запусти оттуда.\n", dest)
	pauseBeforeExit()
	os.Exit(1)
}

func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}

// reExecFrom hands off to the relocated binary and never returns - the
// current process's job ends with relaying the child's exit code.
func reExecFrom(exePath string, args []string) {
	cmd := exec.Command(exePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "перезапуск из новой папки не удался:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// buildReExecArgs carries forward whatever flags were given on the original
// invocation (an unattended flag-driven run shouldn't lose them) plus the
// new config path.
func buildReExecArgs(action, cfgPath string, f setupFlags) []string {
	args := []string{action, "-config", cfgPath}
	if f.nodeSecret != "" {
		args = append(args, "-node-secret", f.nodeSecret)
	}
	if f.directURL != "" {
		args = append(args, "-direct-url", f.directURL)
	}
	if f.iface != "" {
		args = append(args, "-interface", f.iface)
	}
	if f.frontDomain != "" {
		args = append(args, "-front-domain", f.frontDomain)
	}
	if f.frontRealHost != "" {
		args = append(args, "-front-real-host", f.frontRealHost)
	}
	return args
}

// runConfigure resolves the config (same logic as setup) and saves it
// without touching the OS service - for reviewing/editing before install.
func runConfigure(configPath string, f setupFlags) error {
	clearScreen()
	cfg, err := resolveConfig(configPath, f)
	if err != nil {
		return err
	}
	if err := config.Save(configPath, cfg); err != nil {
		return err
	}
	exe := filepath.Base(os.Args[0])
	fmt.Printf("Сохранено в %s. Дальше: %s install && %s start (или просто %s setup)\n", configPath, exe, exe, exe)
	return nil
}

// resolveConfig fills in node_secret/direct_url/interface: from flags where
// given, from the existing config file where already set, prompting
// interactively for whatever's still missing.
//
// Interface selection happens *after* direct_url/front_domain are known and
// *tests real reachability* through each candidate rather than trusting
// "administratively up" - a VPN client, iCloud Private Relay, or any other
// system-level tunnel silently rewriting the default route is extremely
// common, and "up" doesn't mean "this interface's raw path can reach the
// backend". Found this the hard way: an interface that looked fine hung for
// 30s on every single poll. See docs/ARCHITECTURE.md.
func resolveConfig(configPath string, f setupFlags) (config.Config, error) {
	cfg, err := config.Read(configPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("read existing config: %w", err)
	}

	reader := stdin

	switch {
	case f.nodeSecret != "":
		cfg.NodeSecret = f.nodeSecret
	case cfg.NodeSecret == "":
		cfg.NodeSecret = promptString(reader, "Node secret (выдан бекендом при регистрации узла): ")
	}
	switch {
	case f.directURL != "":
		cfg.DirectURL = f.directURL
	case cfg.DirectURL == "":
		cfg.DirectURL = promptString(reader, "URL бекенда - ОБЯЗАТЕЛЬНО со схемой (https://), напр. https://pingachock.rapeer.com:30031: ")
	}
	if f.frontDomain != "" {
		cfg.FrontDomain = f.frontDomain
	}
	if f.frontRealHost != "" {
		cfg.FrontRealHost = f.frontRealHost
	}
	// Only ask interactively if neither flag was given and it's not already
	// configured - this is an optional fallback, most setups don't need it,
	// so don't force the question when a flag-driven unattended run already
	// answered it (or deliberately left it blank).
	if f.frontDomain == "" && f.frontRealHost == "" && cfg.FrontDomain == "" && cfg.FrontRealHost == "" {
		fmt.Println()
		answer := promptString(reader, "Настроить резервный канал через Cloud Run на случай блокировки прямого доступа (direct_url)? Если не знаешь, что это - просто нажми Enter, чтобы пропустить [y/N]: ")
		if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") || strings.EqualFold(answer, "д") || strings.EqualFold(answer, "да") {
			cfg.FrontDomain = promptString(reader, "Домен для маскировки SNI - ТОЛЬКО домен, БЕЗ https:// и БЕЗ порта (порт всегда 443), напр. google.com: ")
			cfg.FrontRealHost = promptString(reader, "Хостнейм Cloud Run сервиса (идёт в заголовок Host) - ТОЛЬКО домен, БЕЗ https://, напр. xxxxx-uc.a.run.app: ")
		}
	}

	// Catch the two most common mistakes automatically rather than letting
	// them silently produce a broken config: direct_url is dialed as a full
	// URL and needs a scheme; front_domain/front_real_host are bare
	// hostnames the code itself dials on :443 or sends as a Host header, so
	// a pasted https:// or trailing path/port there is always wrong.
	cfg.DirectURL = applyNormalize("URL бекенда", cfg.DirectURL, normalizeURL)
	cfg.FrontDomain = applyNormalize("домен для SNI", cfg.FrontDomain, normalizeBareHost)
	cfg.FrontRealHost = applyNormalize("хостнейм Cloud Run", cfg.FrontRealHost, normalizeBareHost)
	warnIfNotHostname("домен для SNI", cfg.FrontDomain)
	warnIfNotHostname("хостнейм Cloud Run", cfg.FrontRealHost)

	// Backend reachability is checked once here, over the OS default route -
	// the same path the running agent actually uses (see program.Start/run).
	// Deliberately NOT tied to interface selection below: that used to probe
	// each candidate interface *bound*, which can fail for reasons that
	// don't affect the unbound default route at all (real production case:
	// a bound path to the fronting proxy's resolved IP got "unreachable
	// network" from Windows while the default route would have gone
	// through fine) - see docs/ARCHITECTURE.md.
	fmt.Print("Проверяю связь с бекендом (через маршрут по умолчанию - так же, как будет работать агент)... ")
	if label, err := probeBackend(cfg); err == nil {
		fmt.Printf("OK (%s)\n", label)
	} else {
		fmt.Printf("не получилось: %v\n", err)
		fmt.Println("ВНИМАНИЕ: проверь node_secret / URL бекенда / резервный канал - возможно, где-то опечатка.")
		fmt.Println("Настройки всё равно будут сохранены - если проблема временная, агент подключится сам.")
	}

	selected, err := chooseInterface(reader, f.iface, cfg)
	if err != nil {
		return config.Config{}, err
	}

	dnsServers, dnsErr := netiface.DNSServers(selected.Name)
	if dnsErr != nil || len(dnsServers) == 0 {
		fmt.Println("Не удалось определить DNS этого интерфейса - будет использован системный резолвер.")
	} else {
		fmt.Printf("DNS этого интерфейса: %s\n", strings.Join(dnsServers, ", "))
	}
	cfg.InterfaceName = selected.Name
	cfg.LocalAddr = selected.Addrs[0].String()
	cfg.DNSServers = dnsServers

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 30
	}
	if cfg.MaxConcurrentChecks <= 0 {
		cfg.MaxConcurrentChecks = 10
	}

	return cfg, nil
}

// probeTimeout is deliberately much shorter than the real 30s client
// timeout used at runtime - a human is waiting on these checks.
const probeTimeout = 8 * time.Second

// probeBackend checks that the backend is reachable over the OS default
// route - unbound, deliberately: this is exactly the path the running
// agent uses (see program.Start/run), so it validates node_secret/
// direct_url/the fronted fallback are actually correct, independent of
// whatever interface gets chosen below for running checks.
func probeBackend(cfg config.Config) (transportLabel string, err error) {
	direct := transport.NewDirect(nil, cfg.DirectURL, cfg.NodeSecret, probeTimeout, nil)
	if pollErr := probePoll(direct); pollErr == nil {
		return "direct", nil
	} else {
		err = pollErr
	}
	if cfg.FrontDomain != "" && cfg.FrontRealHost != "" {
		fronted := transport.NewFronted(nil, cfg.FrontDomain, cfg.FrontRealHost, cfg.NodeSecret, probeTimeout, nil)
		if pollErr := probePoll(fronted); pollErr == nil {
			return "fronted", nil
		}
	}
	return "", err
}

// probePoll calls Poll with a bounded context purely to test reachability.
// Any non-2xx response (e.g. a rejected node_secret) still counts as
// "unreachable" here for simplicity, but the underlying error text is
// preserved and shown to the operator, so a wrong secret reads as
// "status 401: ..." rather than a vague timeout.
func probePoll(tr transport.Transport) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	_, err := tr.Poll(ctx, "probe")
	return err
}

type interfaceProbe struct {
	iface     netiface.Interface
	directOK  bool
	frontedOK bool
	err       error
}

func (p interfaceProbe) ok() bool { return p.directOK || p.frontedOK }

func (p interfaceProbe) describe() string {
	switch {
	case p.directOK:
		return "OK"
	case p.frontedOK:
		return "OK (только резервный путь)"
	default:
		return fmt.Sprintf("нет связи: %v", p.err)
	}
}

// probeInterface tests whether ifc, bound as the local source address, has
// real outbound connectivity - this is only about which interface checks
// (ping/tcp/http/dns against check targets) run through, NOT backend
// communication, which always uses the unbound default route (see
// probeBackend). It uses the backend as a convenient real target that's
// guaranteed to exist rather than a hardcoded third-party host, but a
// failure here says nothing about backend reachability - a bound path can
// be blocked for reasons that don't affect the unbound default route at
// all (see docs/ARCHITECTURE.md), so this is purely informational and
// never blocks setup.
func probeInterface(ifc netiface.Interface, cfg config.Config) interfaceProbe {
	res := interfaceProbe{iface: ifc}
	localIP := ifc.Addrs[0]

	direct := transport.NewDirect(localIP, cfg.DirectURL, cfg.NodeSecret, probeTimeout, nil)
	if err := probePoll(direct); err == nil {
		res.directOK = true
		return res
	} else {
		res.err = err
	}

	if cfg.FrontDomain != "" && cfg.FrontRealHost != "" {
		fronted := transport.NewFronted(localIP, cfg.FrontDomain, cfg.FrontRealHost, cfg.NodeSecret, probeTimeout, nil)
		if err := probePoll(fronted); err == nil {
			res.frontedOK = true
		}
	}
	return res
}

// chooseInterface picks which network interface checks (ping/tcp/http/dns
// against check targets) run through - it has no effect on backend
// communication, which always goes over the OS default route (see
// probeBackend, called earlier in resolveConfig). Connectivity is probed
// purely as an informational signal ("administratively up" isn't the same
// as "actually has a working path out") and never blocks setup - a probe
// failure here doesn't mean the agent can't work, only that checks run
// through this interface might not either, and the operator can always
// pick a different one later via `configure`.
func chooseInterface(reader *bufio.Reader, ifaceFlag string, cfg config.Config) (netiface.Interface, error) {
	ifaces, err := netiface.List()
	if err != nil {
		return netiface.Interface{}, fmt.Errorf("list network interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return netiface.Interface{}, fmt.Errorf("no usable network interfaces found")
	}

	if ifaceFlag != "" && ifaceFlag != "auto" {
		selected, err := pickInterface(ifaceFlag)
		if err != nil {
			return netiface.Interface{}, err
		}
		fmt.Printf("Проверяю связь через %s (для проверок)... ", selected.Name)
		fmt.Println(probeInterface(selected, cfg).describe())
		if !selected.IsPhysical {
			fmt.Printf("ВНИМАНИЕ: %s похож на VPN/туннель, а не физический адаптер - проверки через него\n", selected.Name)
			fmt.Println("покажут то, что видит VPN, а не реальный провайдер. Если это не намеренно,")
			fmt.Println("перезапусти `configure` без -interface и выбери физический вручную.")
		}
		return selected, nil
	}

	fmt.Println("Проверяю связь каждого сетевого интерфейса (для проверок - на связь с бекендом не влияет)...")
	probes := make([]interfaceProbe, len(ifaces))
	for i, ifc := range ifaces {
		fmt.Printf("  [%d] %-15s %-24v %s ... ", i+1, ifc.Name, ifc.Addrs, physicalTag(ifc))
		probes[i] = probeInterface(ifc, cfg)
		fmt.Println(probes[i].describe())
	}

	defaultIdx := pickDefaultInterface(ifaces, probes)

	if ifaceFlag == "auto" {
		idx := defaultIdx
		if idx < 0 {
			idx = 0
		}
		fmt.Printf("Выбран интерфейс %s.\n", ifaces[idx].Name)
		return ifaces[idx], nil
	}

	// Fully interactive with no -interface given at all: let the operator
	// pick from the now-annotated list, defaulting to whatever
	// pickDefaultInterface picked.
	label := fmt.Sprintf("Выбери интерфейс (1-%d)", len(ifaces))
	if defaultIdx >= 0 {
		label += fmt.Sprintf(", Enter = %d", defaultIdx+1)
	}
	label += ": "
	idx := promptIntDefault(reader, label, 1, len(ifaces), defaultIdx+1)
	return ifaces[idx-1], nil
}

// physicalTag labels an interface in the setup listing so the operator can
// see at a glance why one gets preferred over another.
func physicalTag(ifc netiface.Interface) string {
	if ifc.IsPhysical {
		return "(физический)"
	}
	return "(VPN/туннель)"
}

// pickDefaultInterface chooses which interface to default to, in order of
// preference: a physical interface that also showed connectivity, then any
// physical interface (even if the probe was inconclusive - see
// probeInterface's doc comment for why a probe failure doesn't mean much),
// then falls back to any interface that showed connectivity. Physical is
// preferred over VPN/tunnel on principle, not just when the probe agrees -
// running checks through a VPN measures what the VPN's exit node sees, not
// what the local ISP actually does, which defeats the entire point of this
// agent (see docs/ARCHITECTURE.md). Returns -1 if nothing qualifies, in
// which case the operator must pick explicitly.
func pickDefaultInterface(ifaces []netiface.Interface, probes []interfaceProbe) int {
	for i, ifc := range ifaces {
		if ifc.IsPhysical && probes[i].ok() {
			return i
		}
	}
	for i, ifc := range ifaces {
		if ifc.IsPhysical {
			return i
		}
	}
	for i, p := range probes {
		if p.ok() {
			return i
		}
	}
	return -1
}

// normalizeURL ensures a scheme is present, defaulting to https:// - this
// field gets dialed directly as a URL, so forgetting the scheme (the single
// most common mistake here) would otherwise silently break every request.
func normalizeURL(input string) (value string, fixed bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed, false
	}
	return "https://" + trimmed, true
}

// normalizeBareHost strips a scheme/path/port a user might have mistakenly
// included - these fields (SNI disguise domain, Cloud Run Host header
// value) are bare hostnames, never URLs; the code always dials/sends them
// on :443 itself.
func normalizeBareHost(input string) (value string, fixed bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false
	}
	original := trimmed
	if idx := strings.Index(trimmed, "://"); idx >= 0 {
		trimmed = trimmed[idx+3:]
	}
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.LastIndex(trimmed, ":"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return trimmed, trimmed != original
}

func applyNormalize(label, value string, normalize func(string) (string, bool)) string {
	v, fixed := normalize(value)
	if fixed {
		fmt.Printf("(%s: поправил ввод - было %q, стало %q)\n", label, value, v)
	}
	return v
}

// looksLikeHostname is a light sanity check, not real DNS validation - it
// exists specifically to catch the mistake seen in production: a node
// secret (64-char hex string) pasted into front_domain instead of an actual
// domain. Deliberately permissive on everything else - it only warns, never
// blocks, so it doesn't need to fully replicate hostname grammar.
func looksLikeHostname(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		isLetterDigit := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isLetterDigit && r != '-' && r != '.' {
			return false
		}
	}
	if !strings.Contains(s, ".") && len(s) >= 32 && isHexString(s) {
		return false
	}
	return true
}

func isHexString(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func warnIfNotHostname(label, value string) {
	if !looksLikeHostname(value) {
		fmt.Printf("ВНИМАНИЕ: значение %q для %q не похоже на нормальный домен - проверь, не вставил ли туда что-то другое по ошибке (например, node secret).\n", value, label)
	}
}

// pickInterface resolves a -interface flag value to an actual interface:
// "auto" picks the first one that's up, otherwise it must match a name
// exactly.
func pickInterface(name string) (netiface.Interface, error) {
	ifaces, err := netiface.List()
	if err != nil {
		return netiface.Interface{}, fmt.Errorf("list network interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return netiface.Interface{}, fmt.Errorf("no usable network interfaces found")
	}
	if name == "auto" {
		for _, ifc := range ifaces {
			if ifc.IsUp {
				return ifc, nil
			}
		}
		return ifaces[0], nil
	}
	for _, ifc := range ifaces {
		if ifc.Name == name {
			return ifc, nil
		}
	}
	return netiface.Interface{}, fmt.Errorf("interface %q not found (available: see `configure` without -interface to list them)", name)
}

func promptString(r *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptIntDefault prompts for an integer in [min, max]; empty input (bare
// Enter) returns def if it's in range - used to pre-select the interface
// that already tested as reachable.
func promptIntDefault(r *bufio.Reader, label string, min, max, def int) int {
	for {
		fmt.Print(label)
		line, readErr := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" && def >= min && def <= max {
			return def
		}
		n, convErr := strconv.Atoi(line)
		if convErr == nil && n >= min && n <= max {
			return n
		}
		// No valid line and nothing left to read (closed/non-interactive
		// stdin, no usable default) - looping here would spin forever
		// instead of ever finishing. Report it and bail rather than hang.
		if readErr != nil {
			fmt.Println()
			fmt.Println("Не удалось прочитать ввод (нет интерактивного терминала?).")
			fmt.Println("Для запуска без вопросов используй флаги: -node-secret, -direct-url, -interface и т.д.")
			pauseBeforeExit()
			os.Exit(1)
		}
		fmt.Printf("Введи число от %d до %d.\n", min, max)
	}
}

// pauseBeforeExit keeps a double-clicked console window open long enough to
// actually read the output, instead of it flashing shut the instant the
// program finishes.
func pauseBeforeExit() {
	fmt.Println()
	fmt.Print("Нажми Enter для выхода...")
	stdin.ReadString('\n')
}
