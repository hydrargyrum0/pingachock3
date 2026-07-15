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
	"path/filepath"
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
	var localIP net.IP
	if cfg.LocalAddr != "" {
		localIP = net.ParseIP(cfg.LocalAddr)
	}

	direct := transport.NewDirect(localIP, cfg.DirectURL, cfg.NodeSecret, 30*time.Second, p.log)

	var tr transport.Transport = direct
	if cfg.FrontDomain != "" && cfg.FrontRealHost != "" {
		fronted := transport.NewFronted(localIP, cfg.FrontDomain, cfg.FrontRealHost, cfg.NodeSecret, 30*time.Second, p.log)
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

	resolvedConfig := *configPath
	if !filepath.IsAbs(resolvedConfig) {
		if exe, err := os.Executable(); err == nil {
			resolvedConfig = filepath.Join(filepath.Dir(exe), resolvedConfig)
		}
	}

	sf := setupFlags{
		nodeSecret: *nodeSecret, directURL: *directURL, iface: *ifaceFlag,
		frontDomain: *frontDomain, frontRealHost: *frontRealHost,
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
	if err := service.Control(svc, "start"); err != nil {
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
	if err := service.Control(svc, action); err != nil {
		fmt.Printf("%s: %v\n", action, err)
		printIfPermissionError(err)
		return
	}
	fmt.Printf("%s: ok\n", action)
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
		fmt.Printf("Интерфейс:      %s (%s)\n", cfg.InterfaceName, cfg.LocalAddr)
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

// runConfigure resolves the config (same logic as setup) and saves it
// without touching the OS service - for reviewing/editing before install.
func runConfigure(configPath string, f setupFlags) error {
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

// resolveConfig fills in interface/DNS/node_secret/direct_url: from flags
// where given, from the existing config file where already set, prompting
// interactively for whatever's still missing. See internal/netiface for why
// interface/DNS selection matters (a VPN's DNS would otherwise silently
// skew every check).
func resolveConfig(configPath string, f setupFlags) (config.Config, error) {
	cfg, err := config.Read(configPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("read existing config: %w", err)
	}

	reader := stdin

	ifaceName := f.iface
	if ifaceName == "" {
		ifaceName = cfg.InterfaceName // keep previous choice if nothing new specified
	}

	var selected netiface.Interface
	if ifaceName != "" {
		selected, err = pickInterface(ifaceName)
		if err != nil {
			return config.Config{}, err
		}
	} else {
		ifaces, err := netiface.List()
		if err != nil {
			return config.Config{}, fmt.Errorf("list network interfaces: %w", err)
		}
		if len(ifaces) == 0 {
			return config.Config{}, fmt.Errorf("no usable network interfaces found")
		}
		fmt.Println("Доступные сетевые интерфейсы:")
		for i, ifc := range ifaces {
			status := "down"
			if ifc.IsUp {
				status = "up"
			}
			fmt.Printf("  [%d] %-15s %-4s %v\n", i+1, ifc.Name, status, ifc.Addrs)
		}
		idx := promptInt(reader, fmt.Sprintf("Выбери интерфейс (1-%d): ", len(ifaces)), 1, len(ifaces))
		selected = ifaces[idx-1]
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

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 30
	}
	if cfg.MaxConcurrentChecks <= 0 {
		cfg.MaxConcurrentChecks = 10
	}

	return cfg, nil
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

func promptInt(r *bufio.Reader, label string, min, max int) int {
	for {
		fmt.Print(label)
		line, _ := r.ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n >= min && n <= max {
			return n
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
