// The agent doubles as its own installer and setup wizard, aimed at being
// runnable by just right-clicking the exe -> "Run as administrator" on a
// machine that has nothing else on it:
//   - no arguments (bare double-click) -> `setup`: interactively (or via
//     flags, so a shortcut can bake in the answers for unattended rollout)
//     picks a network interface, fills in the rest of agent.json, then
//     installs and starts the native OS service in one go.
//   - `configure` does just the config part, without installing.
//   - `install`/`start`/`stop`/`uninstall` control the native OS service
//     (systemd/launchd/Windows Service via kardianos/service) individually.
//   - `run` runs in the foreground - also what the installed service execs.
//
// Every action pauses for a keypress before exiting (except `run`, which
// blocks until stopped) so a double-clicked console window doesn't just
// flash and vanish before the operator can read what happened.
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

	"pingachock/internal/checks"
	"pingachock/internal/config"
	"pingachock/internal/netiface"
	"pingachock/internal/poller"
	"pingachock/internal/transport"
)

const agentVersion = "0.1.0"

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

	direct := transport.NewDirect(localIP, cfg.DirectURL, cfg.NodeSecret, 30*time.Second)

	var tr transport.Transport = direct
	if cfg.FrontDomain != "" && cfg.FrontRealHost != "" {
		fronted := transport.NewFronted(localIP, cfg.FrontDomain, cfg.FrontRealHost, cfg.NodeSecret, 30*time.Second)
		tr = transport.NewFailover(direct, fronted, p.log)
		p.log.Info("fronted transport configured as fallback", "front_domain", cfg.FrontDomain)
	}

	pl := &poller.Poller{
		Transport:     tr,
		Interval:      time.Duration(cfg.PollIntervalSeconds) * time.Second,
		AgentVersion:  agentVersion,
		MaxConcurrent: cfg.MaxConcurrentChecks,
		NetConfig:     netCfg,
		Log:           p.log,
	}

	p.log.Info("agent starting",
		"direct_url", cfg.DirectURL, "poll_interval", pl.Interval, "node_id", cfg.NodeID,
		"interface", cfg.InterfaceName, "local_addr", cfg.LocalAddr, "dns_servers", cfg.DNSServers)
	pl.Run(ctx)
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
	directURL := fs.String("direct-url", "", "backend URL, e.g. https://backend.example.com - skips the interactive prompt")
	ifaceFlag := fs.String("interface", "", `network interface name, or "auto" for the first one that's up - skips the interactive prompt`)
	frontDomain := fs.String("front-domain", "", "fronted transport: disguise SNI domain (optional)")
	frontRealHost := fs.String("front-real-host", "", "fronted transport: real Cloud Run hostname (optional)")
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

	// Bare double-click / "Run as administrator" with no arguments: do the
	// whole guided setup in one shot rather than requiring separate
	// configure/install/start invocations. Anything invoking this binary
	// with explicit args (including the OS service manager, which always
	// passes "run") is unaffected.
	if action == "" {
		action = "setup"
	}

	switch action {
	case "setup":
		runSetup(resolvedConfig, sf, svc)
		pauseBeforeExit()
	case "configure":
		if err := runConfigure(resolvedConfig, sf); err != nil {
			fmt.Fprintln(os.Stderr, "configure failed:", err)
		}
		pauseBeforeExit()
	case "install", "uninstall", "start", "stop", "restart":
		if err := service.Control(svc, action); err != nil {
			fmt.Fprintf(os.Stderr, "%s failed: %v\n", action, err)
			printIfPermissionError(err)
		} else {
			fmt.Printf("%s: ok\n", action)
		}
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

	reader := bufio.NewReader(os.Stdin)

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
		cfg.DirectURL = promptString(reader, "URL бекенда (direct_url), напр. https://backend.example.com: ")
	}
	if f.frontDomain != "" {
		cfg.FrontDomain = f.frontDomain
	}
	if f.frontRealHost != "" {
		cfg.FrontRealHost = f.frontRealHost
	}

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 30
	}
	if cfg.MaxConcurrentChecks <= 0 {
		cfg.MaxConcurrentChecks = 10
	}

	return cfg, nil
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
	bufio.NewReader(os.Stdin).ReadString('\n')
}
