#!/usr/bin/env python3
"""
Мультитул для поиска хостов, у которых голый HTTP-запрос на порт 443
получает ответ с сигнатурой "The plain HTTP request was sent to
HTTPS port".

Подкоманды:
    check     -- stage 2: подключиться к списку хостов и проверить сигнатуру
                 "plain HTTP was sent to HTTPS port"
    upgrade   -- stage 2: отправить запрос со сменой протокола
                 (Connection: Upgrade) и собрать адреса, которые ответили
                 "101 Switching Protocols" (т.е. согласились на апгрейд)
    discover  -- stage 1: прогнать masscan и найти, у кого открыт порт
    pipeline  -- discover + check (signature или upgrade) одной командой

Никакие диапазоны в скрипт не зашиты -- всё передаётся аргументами
при каждом запуске.

Примеры:
    python check_plain_http.py check --target 10.0.0.1/24
    python check_plain_http.py check --input-file live_hosts.txt --concurrency 2000
    python check_plain_http.py upgrade --input-file live_hosts.txt --upgrade-protocol websocket
    python check_plain_http.py upgrade --target 10.0.0.1/24 --upgrade-protocol "TLS/1.0"
    python check_plain_http.py discover --target 0.0.0.0/1 --rate 100000
    python check_plain_http.py pipeline --target 10.0.0.0/16 --rate 10000 \
        --concurrency 2000 --timeout 3 --log-prefix scan
    python check_plain_http.py pipeline --target 10.0.0.0/16 --check-type upgrade \
        --upgrade-protocol websocket

Отличия от старой версии (важно для больших диапазонов вроде 0.0.0.0/1):
  * цели НЕ разворачиваются в список целиком -- стримятся по одной;
  * НЕ создаётся Task на каждый хост -- работает пул из N воркеров,
    в памяти максимум ~concurrency*2 целей;
  * ответ читается с потолком (--max-body), чтобы злой хост не залил память;
  * masscan по умолчанию получает exclude-лист с приватными/зарезервированными
    диапазонами (RFC1918, loopback, multicast и т.д.);
  * поднимается лимит файловых дескрипторов под заданный concurrency.

discover/pipeline требуют установленный masscan в PATH и права на raw
sockets (root/sudo, либо `sudo setcap cap_net_raw+ep $(which masscan)`).
"""
import argparse
import asyncio
import base64
import ipaddress
import os
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path
from typing import Iterator, Optional

try:
    import resource  # POSIX only
except ImportError:  # Windows
    resource = None

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8")

SIGNATURE = "The plain HTTP request was sent to HTTPS port"
# Дополнительная строка-маркер (нижняя строка error-страницы), которая должна
# встретиться в ответе ОДНОВРЕМЕННО с SIGNATURE -- отсекает не-cloudflare
# хосты с похожим текстом ошибки.
FOOTER = "cloudflare"

# Потолок на объём читаемого ответа. Сигнатура nginx приходит в первых
# байтах, читать больше смысла нет, а злой/кривой хост может лить бесконечно.
DEFAULT_MAX_BODY = 16 * 1024

# Значение заголовка Upgrade по умолчанию для режима "upgrade".
DEFAULT_UPGRADE_PROTOCOL = "websocket"

# Приватные/зарезервированные диапазоны, которые нет смысла (и опасно) сканить.
DEFAULT_EXCLUDES = [
    "0.0.0.0/8",
    "10.0.0.0/8",
    "100.64.0.0/10",
    "127.0.0.0/8",
    "169.254.0.0/16",
    "172.16.0.0/12",
    "192.0.0.0/24",
    "192.0.2.0/24",
    "192.88.99.0/24",
    "192.168.0.0/16",
    "198.18.0.0/15",
    "198.51.100.0/24",
    "203.0.113.0/24",
    "224.0.0.0/4",
    "240.0.0.0/4",
    "255.255.255.255/32",
]


# ---------------------------------------------------------------- stage 2 --

class Stats:
    def __init__(self, total: Optional[int]):
        self.total = total
        self.done = 0
        self.matches = 0
        self.errors = 0
        self.in_flight = 0


def print_line(msg: str) -> None:
    sys.stdout.write("\r\x1b[2K" + msg + "\n")
    sys.stdout.flush()


def _total_str(total: Optional[int]) -> str:
    return str(total) if total is not None else "?"


def render_status(stats: Stats, concurrency: int, start_time: float) -> None:
    elapsed = time.time() - start_time
    rate = stats.done / elapsed if elapsed > 0 else 0.0
    line = (
        f"[status] in-flight: {stats.in_flight}/{concurrency} | "
        f"checked: {stats.done}/{_total_str(stats.total)} | "
        f"matches: {stats.matches} | errors: {stats.errors} | "
        f"{rate:.1f} req/s"
    )
    sys.stdout.write("\r\x1b[2K" + line)
    sys.stdout.flush()


async def status_ticker(stats: Stats, concurrency: int, start_time: float, interval: float = 0.5):
    while True:
        render_status(stats, concurrency, start_time)
        await asyncio.sleep(interval)


def _cidr_host_count(net) -> int:
    """Сколько адресов вернёт net.hosts() -- без материализации."""
    if net.prefixlen >= net.max_prefixlen:          # /32 (или /128)
        return 1
    if net.prefixlen == net.max_prefixlen - 1:       # /31 (или /127)
        return 2
    return net.num_addresses - 2


def count_targets(args: argparse.Namespace) -> Optional[int]:
    """
    Пытаемся дёшево посчитать total для статус-бара.
    Для CIDR -- арифметикой. Для файла НЕ считаем (может быть гигантский
    masscan-вывод), возвращаем None -> в статусе будет '?'.
    """
    if getattr(args, "target", None):
        try:
            net = ipaddress.ip_network(args.target, strict=False)
            return _cidr_host_count(net)
        except ValueError:
            return None
    return None


def iter_targets(args: argparse.Namespace) -> Iterator[str]:
    """
    Стримит цели по одной. Ничего не разворачивает в память целиком --
    даже /1 отдаётся лениво.
    """
    if getattr(args, "target", None):
        network = ipaddress.ip_network(args.target, strict=False)
        for ip in network.hosts():
            yield str(ip)
        return

    with open(args.input_file, "r", encoding="utf-8", errors="replace") as f:
        for raw_line in f:
            line = raw_line.strip()
            if not line or line.startswith("#"):
                continue

            # masscan -oL list format: "open tcp 443 1.2.3.4 1691000000"
            parts = line.split()
            if len(parts) >= 4 and parts[0] == "open":
                line = parts[3]

            try:
                if "/" in line:
                    net = ipaddress.ip_network(line, strict=False)
                    for ip in net.hosts():
                        yield str(ip)
                else:
                    yield str(ipaddress.ip_address(line))
            except ValueError:
                print(f"[skip] can't parse line as IP/CIDR: {line!r}", file=sys.stderr)


def source_description(args: argparse.Namespace) -> str:
    return f"target={args.target}" if getattr(args, "target", None) else f"input_file={args.input_file}"


def raise_fd_limit(target: int) -> None:
    """Best-effort поднять RLIMIT_NOFILE под нужный concurrency."""
    if resource is None:
        return
    want = target + 256  # запас на файлы результатов/логов/stdio
    try:
        soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
        if soft >= want:
            return
        new_soft = want if hard == resource.RLIM_INFINITY else min(want, hard)
        resource.setrlimit(resource.RLIMIT_NOFILE, (new_soft, hard))
        if new_soft < want:
            print(
                f"[warn] ulimit -n поднят только до {new_soft} (hard={hard}); "
                f"для concurrency={target} желательно больше. Подними hard-лимит "
                f"или снизь --concurrency.",
                file=sys.stderr,
            )
    except (ValueError, OSError) as e:
        print(f"[warn] не удалось поднять лимит дескрипторов: {e}", file=sys.stderr)


_UA = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/120.0.6089.3 Safari/537.36"
)


def build_signature_request(ip: str) -> bytes:
    return (
        f"GET / HTTP/1.1\r\n"
        f"Host: {ip}\r\n"
        f"User-Agent: {_UA}\r\n"
        f"Connection: close\r\n"
        f"\r\n"
    ).encode()


def build_upgrade_request(ip: str, upgrade: str) -> bytes:
    """
    Запрос с Connection: Upgrade / Upgrade: <протокол>, вызывающий смену
    протокола -- успех виден по ответу "101 Switching Protocols".

    Для websocket и h2c добавляются заголовки, обязательные для рукопожатия
    (без них нормальный сервер ответит 400/426, а не 101). Для остальных
    токенов (например "TLS/1.0" из RFC 2817) шлём голый Upgrade -- этого
    достаточно.
    """
    headers = [
        "GET / HTTP/1.1",
        f"Host: {ip}",
        f"User-Agent: {_UA}",
    ]
    token = upgrade.lower()
    if token == "websocket":
        key = base64.b64encode(os.urandom(16)).decode()
        headers += [
            "Connection: Upgrade",
            f"Upgrade: {upgrade}",
            "Sec-WebSocket-Version: 13",
            f"Sec-WebSocket-Key: {key}",
        ]
    elif token == "h2c":
        headers += [
            "Connection: Upgrade, HTTP2-Settings",
            f"Upgrade: {upgrade}",
            "HTTP2-Settings: AAMAAABkAAQAAP__",  # пустой валидный SETTINGS-фрейм, base64url
        ]
    else:
        headers += [
            "Connection: Upgrade",
            f"Upgrade: {upgrade}",
        ]
    return ("\r\n".join(headers) + "\r\n\r\n").encode()


def match_signature(text: str) -> bool:
    return SIGNATURE in text and FOOTER in text


def match_switching_protocols(text: str) -> bool:
    """True, если статус-строка ответа -- HTTP/x.y 101 ... (сервер согласился апгрейднуться)."""
    status_line = text.split("\r\n", 1)[0]
    parts = status_line.split(" ", 2)
    return len(parts) >= 2 and parts[1] == "101"


async def probe(ip: str, port: int, timeout: float, max_body: int, stats: Stats,
                 build_request, is_match):
    stats.in_flight += 1
    writer = None
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(ip, port), timeout=timeout
        )
        writer.write(build_request(ip))
        await writer.drain()

        chunks = []
        got = 0
        try:
            while got < max_body:
                chunk = await asyncio.wait_for(reader.read(4096), timeout=timeout)
                if not chunk:
                    break
                chunks.append(chunk)
                got += len(chunk)
        except asyncio.TimeoutError:
            pass  # took what we got

        text = b"".join(chunks).decode(errors="replace")
        return ip, is_match(text), None
    except (asyncio.TimeoutError, OSError) as e:
        return ip, False, str(e)
    except Exception as e:  # не роняем воркер из-за неожиданного
        return ip, False, repr(e)
    finally:
        stats.in_flight -= 1
        if writer is not None:
            writer.close()
            try:
                await writer.wait_closed()
            except OSError:
                pass


async def run_check(args: argparse.Namespace) -> None:
    start_dt = datetime.now()
    ts = start_dt.strftime("%Y%m%d_%H%M%S")
    output_path = f"{args.output_prefix}_{ts}.txt"
    log_path = f"{args.log_prefix}_{ts}.txt" if args.log_prefix else None
    max_body = getattr(args, "max_body", DEFAULT_MAX_BODY)

    mode = getattr(args, "mode", "signature")
    if mode == "upgrade":
        upgrade_protocol = getattr(args, "upgrade_protocol", DEFAULT_UPGRADE_PROTOCOL)
        build_request = lambda ip: build_upgrade_request(ip, upgrade_protocol)  # noqa: E731
        is_match_fn = match_switching_protocols
        match_label = "SWITCH"
        mode_desc = f'upgrade="{upgrade_protocol}" (match = HTTP 101 Switching Protocols)'
    else:
        build_request = build_signature_request
        is_match_fn = match_signature
        match_label = "MATCH"
        mode_desc = f'signature="{SIGNATURE}" footer="{FOOTER}" (match requires both)'

    raise_fd_limit(args.concurrency)

    total = count_targets(args)
    stats = Stats(total)
    log_f = open(log_path, "a", encoding="utf-8") if log_path else None

    def log(msg: str) -> None:
        if log_f:
            log_f.write(msg + "\n")
            log_f.flush()

    header = (
        f"# plain-http-443 scan (mode={mode})\n"
        f"# scan_start={start_dt.isoformat()}\n"
        f"# source={source_description(args)}\n"
        f"# hosts={_total_str(total)} port={args.port} concurrency={args.concurrency} "
        f"timeout={args.timeout} max_body={max_body}\n"
        f"# {mode_desc}\n"
    )
    log(header.rstrip("\n"))

    start = time.time()
    ticker = asyncio.create_task(status_ticker(stats, args.concurrency, start))

    # Очередь ограничена -> в памяти максимум ~concurrency*2 целей,
    # сколько бы миллиардов ни было на входе.
    queue: asyncio.Queue = asyncio.Queue(maxsize=args.concurrency * 2)

    with open(output_path, "a", encoding="utf-8") as out_f:
        out_f.write(header)
        out_f.flush()

        def handle_result(ip: str, is_match: bool, err: Optional[str]) -> None:
            stats.done += 1
            if err:
                stats.errors += 1
            if is_match:
                stats.matches += 1
                out_f.write(f"{ip}\n")
                out_f.flush()
                print_line(f"[{match_label}] {ip}")
                log(f"[{match_label}] {ip}")
            if stats.done % args.progress_every == 0 or stats.done == total:
                elapsed = time.time() - start
                rate = stats.done / elapsed if elapsed > 0 else 0.0
                log(
                    f"[progress] {stats.done}/{_total_str(total)} scanned, "
                    f"{stats.matches} match(es), {stats.errors} error/timeout, "
                    f"{rate:.1f} req/s, {elapsed:.0f}s elapsed"
                )

        async def worker() -> None:
            while True:
                ip = await queue.get()
                try:
                    if ip is None:  # sentinel -> на выход
                        return
                    res_ip, matched, err = await probe(
                        ip, args.port, args.timeout, max_body, stats,
                        build_request, is_match_fn,
                    )
                    handle_result(res_ip, matched, err)
                finally:
                    queue.task_done()

        workers = [asyncio.create_task(worker()) for _ in range(args.concurrency)]

        # Продюсер: стримим цели в очередь, потом кладём по sentinel на воркера.
        try:
            for ip in iter_targets(args):
                await queue.put(ip)
        finally:
            for _ in workers:
                await queue.put(None)

        await asyncio.gather(*workers)

    ticker.cancel()
    try:
        await ticker
    except asyncio.CancelledError:
        pass

    elapsed = time.time() - start
    print_line(
        f"[done] {stats.matches}/{stats.done} matched in {elapsed:.0f}s. "
        f"Results in {output_path}"
    )
    log(f"[done] {stats.matches}/{stats.done} matched in {elapsed:.0f}s. Results in {output_path}")
    if log_f:
        log_f.close()


# ------------------------------------------------------------------------ stage 1 --

def _write_default_exclude_file() -> str:
    fd, path = tempfile.mkstemp(prefix="masscan_exclude_", suffix=".conf")
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        f.write("# auto-generated default excludes (private/reserved)\n")
        for cidr in DEFAULT_EXCLUDES:
            f.write(cidr + "\n")
    return path


def run_masscan(args: argparse.Namespace, live_hosts_path: Path) -> None:
    if not shutil.which(args.masscan_path):
        sys.exit(
            f"[error] masscan executable not found: {args.masscan_path!r}. "
            f"Install it or pass --masscan-path."
        )

    cmd = [args.masscan_path, "-p", str(args.port), "--rate", str(args.rate), "-oL", str(live_hosts_path)]
    if args.target_file:
        cmd += ["-iL", args.target_file]
    else:
        cmd += [args.target]

    # exclude-лист: свой файл имеет приоритет; иначе -- дефолтный, если не отключён.
    tmp_exclude = None
    if getattr(args, "exclude_file", None):
        cmd += ["--excludefile", args.exclude_file]
    elif not getattr(args, "no_default_excludes", False):
        tmp_exclude = _write_default_exclude_file()
        cmd += ["--excludefile", tmp_exclude]

    if args.masscan_args:
        cmd += args.masscan_args.split()

    print(f"[discover] {' '.join(cmd)}")
    try:
        result = subprocess.run(cmd)
    finally:
        if tmp_exclude:
            try:
                os.unlink(tmp_exclude)
            except OSError:
                pass
    if result.returncode != 0:
        sys.exit(f"[error] masscan exited with code {result.returncode}")


def count_open(live_hosts_path: Path) -> int:
    if not live_hosts_path.exists():
        return 0
    with open(live_hosts_path, encoding="utf-8", errors="replace") as f:
        return sum(1 for line in f if line.startswith("open"))


def discover_stage(args: argparse.Namespace, prefix: str) -> tuple[Path, int]:
    ts = datetime.now().strftime("%Y%m%d_%H%M%S")
    live_hosts_path = Path(f"{prefix}_{ts}.txt")
    run_masscan(args, live_hosts_path)
    opened = count_open(live_hosts_path)
    print(f"[discover] {opened} host(s) with port {args.port} open. Saved to {live_hosts_path}")
    return live_hosts_path, opened


# ------------------------------------------------------------------------- cli --

def add_stage2_args(p: argparse.ArgumentParser) -> None:
    p.add_argument("--port", type=int, default=443)
    p.add_argument("--concurrency", type=int, default=300, help="Число воркеров = макс. одновременных соединений")
    p.add_argument("--timeout", type=float, default=3.0, help="Per-connection timeout in seconds")
    p.add_argument("--max-body", type=int, default=DEFAULT_MAX_BODY,
                    help="Потолок читаемого ответа в байтах (сигнатура в первых байтах)")
    p.add_argument("--output-prefix", default="results",
                    help="Results filename prefix; scan start time is appended automatically")
    p.add_argument("--log-prefix", default=None,
                    help="Progress log filename prefix; scan start time is appended automatically")
    p.add_argument("--progress-every", type=int, default=200, help="Log a progress line every N hosts")


def add_upgrade_args(p: argparse.ArgumentParser) -> None:
    p.add_argument(
        "--upgrade-protocol", default=DEFAULT_UPGRADE_PROTOCOL,
        help="Значение заголовка Upgrade: websocket | h2c | \"TLS/1.0\" | произвольный токен "
             "(default: %(default)s)",
    )


def add_masscan_exclude_args(p: argparse.ArgumentParser) -> None:
    p.add_argument("--exclude-file", default=None,
                    help="Свой exclude-файл для masscan (--excludefile). Приоритетнее дефолтного")
    p.add_argument("--no-default-excludes", action="store_true",
                    help="Не подставлять дефолтный exclude приватных/зарезервированных диапазонов")


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="plain-HTTP-on-443 signature scanner (multitool)")
    sub = parser.add_subparsers(dest="command", required=True)

    p_check = sub.add_parser("check", help="Stage 2 only: probe hosts for the signature")
    src = p_check.add_mutually_exclusive_group(required=True)
    src.add_argument("--target", help="IP address or CIDR subnet, e.g. 10.0.0.1/24")
    src.add_argument("--input-file", help="File with IPs/CIDRs/masscan -oL lines, one per line")
    add_stage2_args(p_check)
    p_check.set_defaults(mode="signature")

    p_upg = sub.add_parser(
        "upgrade",
        help="Stage 2 only: send an Upgrade request and collect hosts answering 101 Switching Protocols",
    )
    src_upg = p_upg.add_mutually_exclusive_group(required=True)
    src_upg.add_argument("--target", help="IP address or CIDR subnet, e.g. 10.0.0.1/24")
    src_upg.add_argument("--input-file", help="File with IPs/CIDRs/masscan -oL lines, one per line")
    add_stage2_args(p_upg)
    add_upgrade_args(p_upg)
    p_upg.set_defaults(mode="upgrade")

    p_disc = sub.add_parser("discover", help="Stage 1 only: run masscan to find hosts with the port open")
    src2 = p_disc.add_mutually_exclusive_group(required=True)
    src2.add_argument("--target", help="IP/CIDR passed straight to masscan, e.g. 0.0.0.0/1")
    src2.add_argument("--target-file", help="File of ranges/IPs passed to masscan -iL")
    p_disc.add_argument("--port", type=int, default=443)
    p_disc.add_argument("--rate", type=int, default=10000, help="masscan packets/sec (--rate)")
    p_disc.add_argument("--masscan-path", default="masscan", help="Path to masscan binary if not in PATH")
    p_disc.add_argument("--masscan-args", default="", help="Extra raw arguments appended to the masscan command")
    add_masscan_exclude_args(p_disc)
    p_disc.add_argument("--output-prefix", default="masscan_live",
                         help="Live-hosts filename prefix; scan start time is appended automatically")

    p_pipe = sub.add_parser("pipeline", help="discover + check in one go")
    src3 = p_pipe.add_mutually_exclusive_group(required=True)
    src3.add_argument("--target", help="IP/CIDR passed straight to masscan, e.g. 10.0.0.0/16")
    src3.add_argument("--target-file", help="File of ranges/IPs passed to masscan -iL")
    p_pipe.add_argument("--rate", type=int, default=10000, help="masscan packets/sec (--rate)")
    p_pipe.add_argument("--masscan-path", default="masscan", help="Path to masscan binary if not in PATH")
    p_pipe.add_argument("--masscan-args", default="", help="Extra raw arguments appended to the masscan command")
    add_masscan_exclude_args(p_pipe)
    add_stage2_args(p_pipe)
    p_pipe.add_argument(
        "--check-type", dest="mode", choices=["signature", "upgrade"], default="signature",
        help="Тип проверки на этапе check: signature (дефолт) или upgrade (101 Switching Protocols)",
    )
    add_upgrade_args(p_pipe)

    return parser


def dispatch(args: argparse.Namespace) -> None:
    if args.command in ("check", "upgrade"):
        try:
            asyncio.run(run_check(args))
        except KeyboardInterrupt:
            print("\nInterrupted.")
        return

    if args.command == "discover":
        discover_stage(args, args.output_prefix)
        return

    if args.command == "pipeline":
        live_hosts_path, opened = discover_stage(args, "masscan_live")
        if opened == 0:
            print("[pipeline] nothing to check in stage 2.")
            return

        args.target = None
        args.input_file = str(live_hosts_path)
        try:
            asyncio.run(run_check(args))
        except KeyboardInterrupt:
            print("\nInterrupted.")
        return


# ------------------------------------------------------------------ interactive menu --

def ask(prompt: str, default=None, cast=str):
    suffix = f" [{default}]" if default not in (None, "") else ""
    while True:
        raw = input(f"{prompt}{suffix}: ").strip()
        if not raw:
            if default is None:
                print("  -> обязательное поле, повтори")
                continue
            return default
        try:
            return cast(raw)
        except ValueError:
            print(f"  -> не удалось разобрать как {cast.__name__}, повтори")


def ask_source(target_help: str, file_help: str, target_attr: str, file_attr: str) -> argparse.Namespace:
    ns = argparse.Namespace()
    choice = ask("Источник: [t] target (IP/CIDR) или [f] файл со списком", default="t")
    if choice.lower().startswith("f"):
        setattr(ns, file_attr, ask(file_help))
        setattr(ns, target_attr, None)
    else:
        setattr(ns, target_attr, ask(target_help))
        setattr(ns, file_attr, None)
    return ns


def ask_yes_no(prompt: str, default: bool) -> bool:
    d = "y" if default else "n"
    raw = input(f"{prompt} [{d}]: ").strip().lower()
    if not raw:
        return default
    return raw.startswith("y") or raw.startswith("д")


def menu_check() -> None:
    print("\n-- check: проверка сигнатуры на списке хостов --")
    ns = ask_source(
        "Target IP/CIDR, напр. 10.0.0.1/24", "Путь к файлу со списком IP", "target", "input_file"
    )
    ns.mode = "signature"
    ns.port = ask("Порт", default=443, cast=int)
    ns.concurrency = ask("Concurrency (одновременных соединений)", default=300, cast=int)
    ns.timeout = ask("Timeout, сек", default=3.0, cast=float)
    ns.max_body = ask("Потолок ответа, байт", default=DEFAULT_MAX_BODY, cast=int)
    ns.output_prefix = ask("Префикс файла результатов", default="results")
    log_prefix = input("Префикс лог-файла (пусто = без лога): ").strip()
    ns.log_prefix = log_prefix or None
    ns.progress_every = ask("Строка прогресса в лог каждые N хостов", default=200, cast=int)

    try:
        asyncio.run(run_check(ns))
    except KeyboardInterrupt:
        print("\nInterrupted.")


def menu_upgrade() -> None:
    print("\n-- upgrade: запрос на смену протокола, сбор хостов с ответом 101 Switching Protocols --")
    ns = ask_source(
        "Target IP/CIDR, напр. 10.0.0.1/24", "Путь к файлу со списком IP", "target", "input_file"
    )
    ns.mode = "upgrade"
    ns.port = ask("Порт", default=443, cast=int)
    ns.upgrade_protocol = ask(
        "Upgrade-протокол (websocket / h2c / TLS/1.0 / свой)", default=DEFAULT_UPGRADE_PROTOCOL
    )
    ns.concurrency = ask("Concurrency (одновременных соединений)", default=300, cast=int)
    ns.timeout = ask("Timeout, сек", default=3.0, cast=float)
    ns.max_body = ask("Потолок ответа, байт", default=DEFAULT_MAX_BODY, cast=int)
    ns.output_prefix = ask("Префикс файла результатов", default="results_upgrade")
    log_prefix = input("Префикс лог-файла (пусто = без лога): ").strip()
    ns.log_prefix = log_prefix or None
    ns.progress_every = ask("Строка прогресса в лог каждые N хостов", default=200, cast=int)

    try:
        asyncio.run(run_check(ns))
    except KeyboardInterrupt:
        print("\nInterrupted.")


def menu_discover() -> None:
    print("\n-- discover: masscan-сканирование на открытый порт --")
    ns = ask_source(
        "Target IP/CIDR, напр. 0.0.0.0/1", "Путь к файлу с диапазонами", "target", "target_file"
    )
    ns.port = ask("Порт", default=443, cast=int)
    ns.rate = ask("masscan rate (пакетов/сек)", default=10000, cast=int)
    ns.masscan_path = ask("Путь к masscan", default="masscan")
    ns.masscan_args = input("Доп. аргументы masscan (пусто = нет): ").strip()
    exclude_file = input("Свой exclude-файл (пусто = дефолтный): ").strip()
    ns.exclude_file = exclude_file or None
    ns.no_default_excludes = False if exclude_file else (
        not ask_yes_no("Подставить дефолтный exclude приватных диапазонов?", default=True)
    )
    output_prefix = ask("Префикс файла с живыми хостами", default="masscan_live")

    try:
        discover_stage(ns, output_prefix)
    except SystemExit as e:
        print(f"[error] {e}")


def menu_pipeline() -> None:
    print("\n-- pipeline: discover + check одной командой --")
    ns = ask_source(
        "Target IP/CIDR, напр. 10.0.0.0/16", "Путь к файлу с диапазонами", "target", "target_file"
    )
    ns.rate = ask("masscan rate (пакетов/сек)", default=10000, cast=int)
    ns.masscan_path = ask("Путь к masscan", default="masscan")
    ns.masscan_args = input("Доп. аргументы masscan (пусто = нет): ").strip()
    exclude_file = input("Свой exclude-файл (пусто = дефолтный): ").strip()
    ns.exclude_file = exclude_file or None
    ns.no_default_excludes = False if exclude_file else (
        not ask_yes_no("Подставить дефолтный exclude приватных диапазонов?", default=True)
    )
    ns.port = ask("Порт", default=443, cast=int)
    ns.mode = ask("Тип проверки stage 2 (signature / upgrade)", default="signature")
    if ns.mode == "upgrade":
        ns.upgrade_protocol = ask(
            "Upgrade-протокол (websocket / h2c / TLS/1.0 / свой)", default=DEFAULT_UPGRADE_PROTOCOL
        )
    else:
        ns.upgrade_protocol = DEFAULT_UPGRADE_PROTOCOL
    ns.concurrency = ask("Concurrency stage 2 (одновременных соединений)", default=300, cast=int)
    ns.timeout = ask("Timeout stage 2, сек", default=3.0, cast=float)
    ns.max_body = ask("Потолок ответа, байт", default=DEFAULT_MAX_BODY, cast=int)
    ns.output_prefix = ask("Префикс файла результатов", default="results")
    log_prefix = input("Префикс лог-файла (пусто = без лога): ").strip()
    ns.log_prefix = log_prefix or None
    ns.progress_every = ask("Строка прогресса в лог каждые N хостов", default=200, cast=int)

    try:
        dispatch(argparse.Namespace(command="pipeline", **vars(ns)))
    except SystemExit as e:
        print(f"[error] {e}")
    except KeyboardInterrupt:
        print("\nInterrupted.")


MENU_ACTIONS = {
    "1": ("check    -- проверить сигнатуру на списке хостов", menu_check),
    "2": ("upgrade  -- запрос на смену протокола, собрать хосты с ответом 101", menu_upgrade),
    "3": ("discover -- найти живые хосты через masscan", menu_discover),
    "4": ("pipeline -- discover + check одной командой", menu_pipeline),
}


def interactive_menu() -> None:
    while True:
        print("\n=== plain-http-443 scanner ===")
        for key, (label, _) in MENU_ACTIONS.items():
            print(f"  {key}) {label}")
        print("  q) выход")
        choice = input("Выбор: ").strip().lower()

        if choice in ("q", "quit", "exit"):
            break

        action = MENU_ACTIONS.get(choice)
        if not action:
            print("Не понял выбор, попробуй ещё раз.")
            continue

        try:
            action[1]()
        except Exception as e:
            print(f"[error] {e}")


def main() -> None:
    if len(sys.argv) == 1:
        interactive_menu()
        return

    parser = build_arg_parser()
    args = parser.parse_args()
    dispatch(args)


if __name__ == "__main__":
    main()
