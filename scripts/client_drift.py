#!/usr/bin/env python3
"""Keep the proxy's copy of agy, OpenCode and Claude Code wire identity current.

The proxy imitates three clients byte for byte (AGENTS.md, ADR 0004). Each of
them updates itself, and an update can move a version string, a header, the
beta list, a TLS ClientHello or the cch derivation. This script notices an
update, records what the new client sends to its real upstream, and compares
that with what the proxy pins.

  status [--json]  is a run due? (an installed client binary changed)
  check            capture and compare against this checkout; prints the
                   report and changes nothing
  run              check against fork/main in a private worktree, update the
                   pins, verify, push the drift branch to the fork and open or
                   refresh its PR
  ack              mark the last result as shown (the omp extension calls it)

Capture runs each real client through mitmdump with scripts/mitm_header_dump.py,
which redacts credentials and records body fingerprints, never bodies. The
clients run with a scrubbed environment and throwaway config dirs, so user
settings (a base URL that points at this proxy, disabled betas) stay out of the
capture. The proxy's own Cloud Code requests go through the same interceptor via
tools/clientprobe, and the same tool checks that Google accepts the WebUI
sign-in client and its loopback redirect.

Updated without a human: the pins in PINS (versions, user-agent parts, the beta
list, Stainless header values, JA3/JA4 literals), the replayed ClientHello
files in HELLOS, and the baseline captures under .reference/client-drift/.
Anything else becomes a manual item in the PR body, and the PR stays a draft.

State lives outside the repo, in $CLIENT_DRIFT_STATE_DIR (default
~/.cache/antigravity-proxy/client-drift).
"""

from __future__ import annotations

import argparse
import collections
import dataclasses
import datetime
import fcntl
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Callable

SCRIPTS = Path(__file__).resolve().parent
REPO = SCRIPTS.parent
sys.path.insert(0, str(SCRIPTS))

import diff_claude_code_identity as ccdiff  # noqa: E402
import tls_hello  # noqa: E402

REMOTE = "fork"
# Overridable so a branch that is not merged yet can run the whole pipeline.
BASE_BRANCH = os.environ.get("CLIENT_DRIFT_BASE", "main")
DRIFT_BRANCH = os.environ.get("CLIENT_DRIFT_BRANCH", "chore/client-identity-drift")
BASELINE_DIR = ".reference/client-drift"
PROMPT = "Reply with exactly OK"
CLIENT_TIMEOUT = 150
RETRY_AFTER_FAILURE = 6 * 3600


def state_dir() -> Path:
    explicit = os.environ.get("CLIENT_DRIFT_STATE_DIR")
    if explicit:
        return Path(explicit)
    cache = os.environ.get("XDG_CACHE_HOME") or str(Path.home() / ".cache")
    return Path(cache) / "antigravity-proxy" / "client-drift"


def log(message: str) -> None:
    print(f"[{datetime.datetime.now().strftime('%H:%M:%S')}] {message}", flush=True)


# ---------------------------------------------------------------- clients


@dataclasses.dataclass(frozen=True)
class Client:
    name: str
    binary: str
    hosts: tuple[str, ...]
    # Lowercased header names whose values legitimately differ between runs.
    dynamic_headers: frozenset[str]


CLIENTS = {
    "agy": Client(
        "agy", "agy",
        ("daily-cloudcode-pa.googleapis.com", "cloudcode-pa.googleapis.com"),
        frozenset({"authorization", "content-length", "host"}),
    ),
    "claude": Client(
        "claude", "claude", ("api.anthropic.com",), frozenset(ccdiff.DYNAMIC_VALUE),
    ),
    "opencode": Client(
        "opencode", "opencode", ("opencode.ai",),
        frozenset({
            "authorization", "content-length", "host", "b3", "traceparent",
            "x-opencode-session", "x-session-affinity", "x-session-id",
            # A hash of the working directory's project root.
            "x-opencode-project",
        }),
    ),
}


def binary_path(client: Client) -> str | None:
    # The proxy reads agy credentials from the same executable (agy_oauth.go).
    if client.name == "agy" and os.environ.get("AGY_BINARY_PATH"):
        return os.environ["AGY_BINARY_PATH"]
    found = shutil.which(client.binary)
    return os.path.realpath(found) if found else None


def signature() -> dict:
    """Path, size and mtime of each installed client: cheap, and it moves on update."""
    out: dict = {}
    for name, client in CLIENTS.items():
        path = binary_path(client)
        try:
            info = os.stat(path) if path else None
        except OSError:
            info = None
        out[name] = {"path": path, "size": info.st_size, "mtime": int(info.st_mtime)} if info else None
    return out


def installed_version(client: Client) -> str | None:
    path = binary_path(client)
    if not path:
        return None
    try:
        done = subprocess.run([path, "--version"], capture_output=True, text=True,
                              timeout=30, stdin=subprocess.DEVNULL)
    except (OSError, subprocess.TimeoutExpired):
        return None
    match = re.search(r"\d+\.\d+\.\d+", done.stdout + done.stderr)
    return match.group(0) if match else None


# ---------------------------------------------------------------- capture


@dataclasses.dataclass
class Capture:
    """What one interceptor run recorded."""

    records: list[dict]
    hellos: dict[str, bytes]  # JA4 -> raw ClientHello
    runs: list[dict] = dataclasses.field(default_factory=list)

    def first(self, method: str, path_prefix: str) -> dict | None:
        for record in self.records:
            if record.get("method") == method and str(record.get("path", "")).startswith(path_prefix):
                return record
        return None


def header(record: dict | None, name: str) -> str | None:
    if not record:
        return None
    for key, value in record.get("headers", []):
        if key.lower() == name:
            return value if isinstance(value, str) else None
    return None


def scrubbed_env(proxy_url: str, extra: dict[str, str]) -> dict[str, str]:
    """A client environment with nothing of the user's session in it."""
    env = {key: os.environ[key] for key in ("HOME", "PATH", "USER", "LOGNAME", "LANG", "TMPDIR", "SHELL")
           if key in os.environ}
    env.update({
        "TERM": "dumb",
        "HTTPS_PROXY": proxy_url, "https_proxy": proxy_url,
        "HTTP_PROXY": proxy_url, "http_proxy": proxy_url,
        "NO_PROXY": "localhost,127.0.0.1", "no_proxy": "localhost,127.0.0.1",
    })
    env.update(extra)
    return env


def run_command(argv: list[str], env: dict[str, str], cwd: Path, timeout: int = CLIENT_TIMEOUT) -> dict:
    started = time.monotonic()
    stdout = ""
    try:
        done = subprocess.run(argv, env=env, cwd=cwd, capture_output=True, text=True,
                              timeout=timeout, stdin=subprocess.DEVNULL)
        code, stdout, output = done.returncode, done.stdout, done.stdout + done.stderr
    except subprocess.TimeoutExpired as expired:
        code = "timeout"
        output = expired.stdout.decode("utf-8", "replace") if isinstance(expired.stdout, bytes) else (expired.stdout or "")
    except OSError as error:
        code, output = "error", str(error)
    tail = " | ".join(line for line in output.strip().splitlines()[-3:])
    return {"argv": " ".join([Path(argv[0]).name, *argv[1:]]), "exit": code,
            "seconds": round(time.monotonic() - started, 1), "tail": tail[-300:], "stdout": stdout[-4000:]}


class Interceptor:
    """mitmdump with the redacting header-dump addon, for one capture phase."""

    def __init__(self, name: str, hosts: tuple[str, ...], run_dir: Path, root: Path) -> None:
        self.dump = run_dir / f"{name}.jsonl"
        self.hello_dir = run_dir / f"{name}-hellos"
        self.log_path = run_dir / f"{name}-mitmdump.log"
        self.hosts = hosts
        self.addon = root / "scripts" / "mitm_header_dump.py"
        self.confdir = state_dir() / "mitmproxy"
        self.ca = str(self.confdir / "mitmproxy-ca-cert.pem")
        self.port = 0
        self.process: subprocess.Popen | None = None

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def __enter__(self) -> Interceptor:
        mitmdump = shutil.which("mitmdump")
        if not mitmdump:
            raise RuntimeError("mitmdump not found (brew install mitmproxy)")
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            self.port = probe.getsockname()[1]
        allow = "^(" + "|".join(re.escape(host) for host in self.hosts) + "):443$"
        env = dict(os.environ, MITM_DUMP_HOSTS=",".join(self.hosts), MITM_DUMP_REQUEST_BODY="1",
                   MITM_DUMP_OUT=str(self.dump), MITM_DUMP_HELLO_DIR=str(self.hello_dir))
        self.confdir.mkdir(parents=True, exist_ok=True)
        self.process = subprocess.Popen(
            [mitmdump, "--listen-host", "127.0.0.1", "--listen-port", str(self.port),
             "--allow-hosts", allow, "--set", f"confdir={self.confdir}", "-s", str(self.addon)],
            env=env, stdout=open(self.log_path, "w"), stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"mitmdump exited on startup; see {self.log_path}")
            if os.path.exists(self.ca):
                try:
                    socket.create_connection(("127.0.0.1", self.port), timeout=0.5).close()
                    return self
                except OSError:
                    pass
            time.sleep(0.2)
        raise RuntimeError(f"mitmdump did not start; see {self.log_path}")

    def __exit__(self, *_exc) -> None:
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()

    def result(self, runs: list[dict]) -> Capture:
        records = []
        if self.dump.exists():
            records = [json.loads(line) for line in self.dump.read_text(encoding="utf-8").splitlines() if line.strip()]
        hellos = {}
        if self.hello_dir.is_dir():
            hellos = {path.stem: path.read_bytes() for path in self.hello_dir.glob("*.bin")}
        return Capture(records, hellos, runs)

    def root_bundle(self) -> str:
        """The interceptor CA plus the system roots, for SSL_CERT_FILE.

        Go replaces its root pool with SSL_CERT_FILE, so the CA alone breaks
        every host the interceptor tunnels instead of terminating (agy's token
        refresh, its userinfo eligibility check).
        """
        bundle = self.dump.with_name(self.dump.stem + "-roots.pem")
        system = next((Path(p) for p in ("/etc/ssl/cert.pem", "/etc/ssl/certs/ca-certificates.crt",
                                          "/etc/pki/tls/certs/ca-bundle.crt") if Path(p).is_file()), None)
        if system is None:
            raise RuntimeError("no system CA bundle found to combine with the interceptor CA")
        bundle.write_text(Path(self.ca).read_text() + "\n" + system.read_text())
        return str(bundle)


def capture_client(name: str, run_dir: Path, root: Path) -> Capture:
    client = CLIENTS[name]
    path = binary_path(client)
    if not path:
        return Capture([], {}, [{"argv": client.binary, "exit": "missing", "seconds": 0, "tail": "not installed"}])
    work = Path(tempfile.mkdtemp(prefix=f"client-drift-{name}-", dir=run_dir))
    with Interceptor(name, client.hosts, run_dir, root) as interceptor:
        runs = []
        if name == "agy":
            env = scrubbed_env(interceptor.url, {"SSL_CERT_FILE": interceptor.root_bundle()})
            runs.append(run_command([path, "models"], env, work))
            runs.append(run_command([path, "-p", PROMPT, "--print-timeout", "90s"], env, work))
        elif name == "claude":
            token_file = Path(os.environ.get("CLIENT_DRIFT_CLAUDE_TOKEN_FILE", Path.home() / ".claude-oat"))
            if not token_file.is_file():
                runs.append({"argv": "claude", "exit": "skipped", "seconds": 0,
                             "tail": f"no OAuth token file at {token_file} (claude setup-token)"})
            else:
                # The same flags as the committed container captures
                # (scripts/capture-claude-code-headers.sh), so they compare.
                env = scrubbed_env(interceptor.url, {
                    "NODE_EXTRA_CA_CERTS": interceptor.ca,
                    "CLAUDE_CONFIG_DIR": str(work / "config"),
                    "CLAUDE_CODE_OAUTH_TOKEN": token_file.read_text(encoding="utf-8").strip(),
                    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                    "DISABLE_AUTOUPDATER": "1",
                    "DISABLE_TELEMETRY": "1",
                })
                runs.append(run_command([path, "--print", PROMPT], env, work))
        elif name == "opencode":
            model = os.environ.get("CLIENT_DRIFT_OPENCODE_MODEL", "mimo-v2.6-flash-free")
            env = scrubbed_env(interceptor.url, {
                "NODE_EXTRA_CA_CERTS": interceptor.ca,
                "XDG_CONFIG_HOME": str(work / "config"), "XDG_DATA_HOME": str(work / "data"),
                "XDG_STATE_HOME": str(work / "state"), "XDG_CACHE_HOME": str(work / "cache"),
            })
            runs.append(run_command([path, "run", "--standalone", "-m", f"opencode/{model}", PROMPT], env, work))
        time.sleep(1)  # let the addon flush the last response
    capture = interceptor.result(runs)
    shutil.rmtree(work, ignore_errors=True)
    return capture


def probe_root(root: Path) -> Path:
    """The tree whose tools/clientprobe runs: root, or this script's own tree
    while root predates the tool."""
    return root if (root / "tools" / "clientprobe").is_dir() else REPO


def capture_proxy_cloudcode(run_dir: Path, root: Path) -> Capture:
    with Interceptor("proxy-cloudcode", CLIENTS["agy"].hosts, run_dir, root) as interceptor:
        env = dict(os.environ, SSL_CERT_FILE=interceptor.ca)
        run = run_command(["go", "run", "./tools/clientprobe", "cloudcode", "-proxy", interceptor.url],
                          env, probe_root(root), timeout=300)
        time.sleep(1)
    return interceptor.result([run])


def probe_oauth(root: Path) -> dict:
    done = run_command(["go", "run", "./tools/clientprobe", "oauth"], dict(os.environ), probe_root(root), timeout=300)
    if done["exit"] != 0:
        return {"error": f"clientprobe oauth failed: {done['tail']}"}
    try:
        return json.loads(done["stdout"])
    except ValueError:
        return {"error": f"clientprobe oauth printed {done['stdout']!r}"}


# ---------------------------------------------------------------- pins

VALUE, ANY, SPACE = "<<V>>", "<<X>>", "<<S>>"
JA4 = r"[tqd][0-9a-z]{9}_[0-9a-f]{12}_[0-9a-f]{12}"
JA3 = r"[0-9a-f]{32}"
SEMVER = r"\d+\.\d+\.\d+"


@dataclasses.dataclass(frozen=True)
class Site:
    """One place a pin is written: a literal template around <<V>>.

    <<X>> matches any run of characters up to a quote or newline and <<S>> any
    run of blanks, for the parts of a site that are not the value.
    """

    path: str
    template: str
    count: int = 1

    def regex(self, value_pattern: str) -> re.Pattern:
        parts = []
        for part in re.split(r"(<<[VXS]>>)", self.template):
            if part == VALUE:
                parts.append(f"(?P<value>{value_pattern})")
            elif part == ANY:
                parts.append(r'[^"\n]*')
            elif part == SPACE:
                parts.append(r"[ \t]+")
            else:
                parts.append(re.escape(part))
        return re.compile("".join(parts))


def _same(text: str) -> object:
    return text


@dataclasses.dataclass(frozen=True)
class Pin:
    """A wire value the proxy copies from a client, and every site that copies it.

    observe reads the value from a capture of the real client. A pin without
    one follows another change (a JA4 literal follows its hello file).
    """

    name: str
    client: str
    pattern: str
    sites: tuple[Site, ...]
    observe: Callable[[Capture], object] | None = None
    decode: Callable[[str], object] = _same
    encode: Callable[[object], str] = _same


class PinError(Exception):
    pass


def read_pin(root: Path, pin: Pin) -> object:
    values = set()
    for site in pin.sites:
        text = (root / site.path).read_text(encoding="utf-8")
        found = [match.group("value") for match in site.regex(pin.pattern).finditer(text)]
        if len(found) != site.count:
            raise PinError(f"{pin.name}: {site.path} has {len(found)} match(es) of "
                           f"{site.template!r}, expected {site.count}")
        values.update(found)
    if len(values) != 1:
        raise PinError(f"{pin.name}: its sites disagree: {sorted(values)}")
    return pin.decode(values.pop())


def write_pin(root: Path, pin: Pin, value: object) -> list[str]:
    """Write value at every site; refuses when a site is ambiguous or out of step."""
    encoded = pin.encode(value)
    if not re.fullmatch(pin.pattern, encoded):
        raise PinError(f"{pin.name}: {encoded!r} does not match {pin.pattern!r}")
    read_pin(root, pin)
    changed = set()
    for site in pin.sites:
        path = root / site.path
        text = path.read_text(encoding="utf-8")

        def swap(match: re.Match) -> str:
            whole, start = match.group(0), match.start()
            return whole[: match.start("value") - start] + encoded + whole[match.end("value") - start:]

        updated = site.regex(pin.pattern).sub(swap, text)
        if updated != text:
            path.write_text(updated, encoding="utf-8")
            changed.add(site.path)
    return sorted(changed)


AGY_USER_AGENT = re.compile(
    r"^antigravity/cli/(?P<version>\S+) \(aidev_client; os_type=(?P<os>[^;]+); "
    r"arch=(?P<arch>[^;]+); cl=(?P<cl>\d+); auth_method=(?P<auth>[^)]+)\)$")


def agy_user_agent(capture: Capture, part: str) -> str | None:
    for record in capture.records:
        match = AGY_USER_AGENT.match(header(record, "user-agent") or "")
        if match:
            return match.group(part)
    return None


def claude_messages(capture: Capture) -> dict | None:
    return capture.first("POST", "/v1/messages")


def claude_version(capture: Capture) -> str | None:
    match = re.match(r"claude-cli/(\S+) \(", header(claude_messages(capture), "user-agent") or "")
    return match.group(1) if match else None


def claude_header(name: str) -> Callable[[Capture], str | None]:
    return lambda capture: header(claude_messages(capture), name)


def claude_betas(capture: Capture) -> list[str] | None:
    value = header(claude_messages(capture), "anthropic-beta")
    return value.split(",") if value else None


def opencode_version(capture: Capture) -> str | None:
    match = re.match(r"opencode/latest/([^/]+)/", header(capture.first("POST", "/zen/"), "user-agent") or "")
    return match.group(1) if match else None


def agy_ja4(capture: Capture) -> str | None:
    for record in capture.records:
        if (record.get("tls") or {}).get("ja4"):
            return record["tls"]["ja4"]
    return None


BETA_LIST = r'(?:\t"[^"\n]+",\n)*\t"[^"\n]+",'

PINS: tuple[Pin, ...] = (
    Pin("agy.version", "agy", SEMVER, (
        Site("internal/cloudcode/client.go", 'DefaultUserAgentVersion = "<<V>>"'),
    ), lambda c: agy_user_agent(c, "version")),
    Pin("agy.cl", "agy", r"\d+", (
        Site("internal/cloudcode/client.go", 'AgyBuildCL<<S>>= "<<V>>"'),
    ), lambda c: agy_user_agent(c, "cl")),
    Pin("agy.auth_method", "agy", r"[a-z_]+", (
        Site("internal/cloudcode/client.go", 'AgyAuthMethod<<S>>= "<<V>>"'),
    ), lambda c: agy_user_agent(c, "auth")),
    # Follows agy only when this build's own hello matches it (analyze_agy):
    # the Go toolchain decides it, not a literal.
    Pin("agy.ja4", "agy", JA4, (
        Site("scripts/verify-ja4.sh", 'EXPECTED_JA4="<<V>>"'),
        Site("scripts/verify-ja4.sh", "#   <<V>>"),
        Site("README.md", "JA4 fingerprint (`<<V>>`)"),
        Site("README.md", "- JA4 `<<V>>`"),
    ), agy_ja4),
    Pin("claude.version", "claude", SEMVER, (
        Site("internal/ccidentity/defaults.go", 'ClientVersion = "<<V>>"'),
        Site("internal/ccidentity/defaults.go", 'MessagesUserAgent = "claude-cli/<<V>> (external, sdk-cli)"'),
        Site("internal/ccidentity/defaults.go", 'DiscoveryUserAgent = "claude-code/<<V>>"'),
        Site("internal/ccidentity/defaults.go", '"claude-cli/<<V>> (external, cli)"'),
        Site("internal/ccidentity/apply_test.go", '"claude-cli/<<V>> (external, cli)"'),
        Site("internal/api/ccidentity_proxy_test.go", '"claude-cli/<<V>> (external, cli)"'),
        Site("internal/api/ccidentity_proxy_test.go", 'cc_version=<<V>>.")'),
        Site("internal/claudecode/client_test.go", 'cc_version=<<V>>.")'),
        Site("internal/claudecode/client_test.go", "records claude-code/<<V>> on every GET"),
        Site("internal/webui/public/views/settings.html", 'placeholder="claude-cli/<<V>> (external, sdk-cli)"'),
        Site(".reference/claude-code-tls-20261020.json", '"client": "claude-code/<<V>>"'),
    ), claude_version),
    Pin("claude.betas", "claude", BETA_LIST, (
        Site("internal/ccidentity/defaults.go", "var Betas = []string{\n<<V>>\n}"),
    ), claude_betas,
        decode=lambda text: re.findall(r'"([^"]+)"', text),
        encode=lambda values: "\n".join(f'\t"{value}",' for value in values)),
    Pin("claude.stainless_package", "claude", SEMVER, (
        Site("internal/ccidentity/defaults.go", '{"X-Stainless-Package-Version", "<<V>>"}'),
        Site("internal/ccidentity/apply_test.go", '"X-Stainless-Package-Version":<<S>>"<<V>>"'),
    ), claude_header("x-stainless-package-version")),
    Pin("claude.runtime_version", "claude", r"v\d+\.\d+\.\d+", (
        Site("internal/ccidentity/defaults.go", 'return "<<V>>"'),
        Site("internal/ccidentity/apply_test.go", '"X-Stainless-Runtime-Version":<<S>>"<<V>>"'),
        Site("internal/webui/public/views/settings.html", 'placeholder="<<V>>"'),
    ), claude_header("x-stainless-runtime-version")),
    Pin("claude.fetch.ja4", "claude", JA4, (
        Site(".reference/claude-code-tls-20261020.json", '"fetch": {\n      "ja4": "<<V>>"'),
        Site("internal/transport/claudecode.go", "/api/event_logging/v2/batch (JA4 <<V>>);"),
    )),
    Pin("claude.fetch.ja3", "claude", JA3, (
        Site(".reference/claude-code-tls-20261020.json", '"fetch": {\n      "ja4": "<<X>>",\n      "ja3": "<<V>>"'),
    )),
    Pin("claude.node.ja4", "claude", JA4, (
        Site(".reference/claude-code-tls-20261020.json", '"node": {\n      "ja4": "<<V>>"'),
        Site("internal/transport/claudecode.go", "batches (JA4 <<V>>)."),
    )),
    Pin("claude.node.ja3", "claude", JA3, (
        Site(".reference/claude-code-tls-20261020.json", '"node": {\n      "ja4": "<<X>>",\n      "ja3": "<<V>>"'),
    )),
    # The baseline scripts/verify-claude-code-identity.sh holds the proxy to.
    # It moves to .reference/client-drift/claude.jsonl with the first refresh.
    Pin("claude.gate_baseline", "claude", r"\.reference/[A-Za-z0-9_./-]+\.jsonl", (
        Site("scripts/verify-claude-code-identity.sh", 'BASELINE="${CLAUDE_IDENTITY_BASELINE:-${REPO_ROOT}/<<V>>}"'),
    )),
    Pin("opencode.version", "opencode", SEMVER, (
        Site("internal/zen/harness.go", 'DefaultVersion = "<<V>>"'),
        Site("internal/zen/harness.go", "header set is the one OpenCode <<V>> sends"),
        Site("internal/zen/harness_test.go", 'DefaultVersion != "<<V>>"'),
        Site("internal/zen/harness_test.go", 'DefaultVersion, "<<V>>"'),
        Site("internal/config/config_test.go", 'cfg.Zen.Harness.Version != "<<V>>"'),
        Site("internal/config/config_test.go", 'cfg.Zen.Harness.Version, "<<V>>"'),
        Site("internal/config/config_test.go", 'h["version"] != "<<V>>"'),
        Site("internal/webui/tests/zen-harness-identity.test.mjs",
             "version: '<<V>>', client: 'cli', project: 'global'", count=2),
        Site("internal/webui/tests/zen-harness-identity.test.mjs", "harness.version, '<<V>>'"),
        Site("README.md", "OpenCode <<V>> header set"),
        Site("README.md", '"version": "<<V>>", "client"'),
    ), opencode_version),
    Pin("zen.ja4", "opencode", JA4, (
        Site("scripts/verify-zen-tls.sh", "#   JA4 <<V>>"),
        Site("scripts/verify-zen-tls.sh", 'EXPECTED_JA4="<<V>>"'),
    )),
    Pin("zen.ja3", "opencode", JA3, (
        Site("scripts/verify-zen-tls.sh", "#   JA3 <<V>>"),
        Site("scripts/verify-zen-tls.sh", 'EXPECTED_JA3="<<V>>"'),
        Site("internal/zen/tls_test.go", 'want != "<<V>>"'),
        Site("internal/zen/tls_test.go", "want baseline <<V>>"),
        Site("README.md", "JA3 `<<V>>`"),
    )),
)
PIN = {pin.name: pin for pin in PINS}


@dataclasses.dataclass(frozen=True)
class Hello:
    """A ClientHello the proxy replays byte for byte, and the record that shows it."""

    name: str
    client: str
    path: str
    ja4_pin: str
    ja3_pin: str
    select: Callable[[Capture], dict | None]


HELLOS: tuple[Hello, ...] = (
    Hello("claude.fetch", "claude", "internal/transport/claude-code-fetch-clienthello.bin",
          "claude.fetch.ja4", "claude.fetch.ja3", claude_messages),
    Hello("claude.node", "claude", "internal/transport/claude-code-node-clienthello.bin",
          "claude.node.ja4", "claude.node.ja3", lambda c: c.first("GET", "/api/")),
    Hello("zen", "opencode", "internal/zen/opencode-clienthello.bin",
          "zen.ja4", "zen.ja3", lambda c: c.first("POST", "/zen/")),
)


# ---------------------------------------------------------------- analysis


@dataclasses.dataclass
class Result:
    client: str
    installed: str | None = None
    capture: Capture | None = None
    pin_changes: list[tuple[Pin, object, object]] = dataclasses.field(default_factory=list)
    hello_changes: list[tuple[Hello, dict, dict, bytes]] = dataclasses.field(default_factory=list)
    manual: list[str] = dataclasses.field(default_factory=list)
    info: list[str] = dataclasses.field(default_factory=list)
    write_baseline: bool = False

    @property
    def changes(self) -> bool:
        return bool(self.pin_changes or self.hello_changes or self.write_baseline)


def substitutions(changes: list[tuple[Pin, object, object]]) -> list[tuple[str, str]]:
    pairs = []
    for _, old, new in changes:
        if isinstance(old, list):
            pairs.append((",".join(old), ",".join(new)))
        else:
            pairs.append((str(old), str(new)))
    return pairs


def explained(pairs: list[tuple[str, str]], before: object, after: object) -> bool:
    """Whether the pin changes alone turn the baseline value into the observed one."""
    if not isinstance(before, str) or not isinstance(after, str):
        return False
    for old, new in pairs:
        before = before.replace(old, new)
    return before == after


def request_key(record: dict) -> tuple[str, str]:
    return record.get("method", ""), str(record.get("path", "")).split("?")[0]


def compare_records(label: str, base: dict, seen: dict, dynamic: frozenset[str],
                    pairs: list[tuple[str, str]]) -> list[str]:
    """Wire differences between two records of the same request."""
    drifts = []
    if base.get("host") != seen.get("host"):
        drifts.append(f"{label}: host {base.get('host')} -> {seen.get('host')}")
    if base.get("http_version") != seen.get("http_version"):
        drifts.append(f"{label}: {base.get('http_version')} -> {seen.get('http_version')}")
    base_headers = [(name.lower(), value) for name, value in base.get("headers", [])]
    seen_headers = [(name.lower(), value) for name, value in seen.get("headers", [])]
    base_names = [name for name, _ in base_headers]
    seen_names = [name for name, _ in seen_headers]
    missing = [name for name in base_names if name not in seen_names]
    extra = [name for name in seen_names if name not in base_names]
    if missing:
        drifts.append(f"{label}: headers no longer sent: {', '.join(missing)}")
    if extra:
        drifts.append(f"{label}: new headers: {', '.join(extra)}")
    shared = [name for name in base_names if name in seen_names]
    if shared != [name for name in seen_names if name in base_names]:
        drifts.append(f"{label}: header order {shared} -> {[n for n in seen_names if n in base_names]}")
    base_values, seen_values = dict(base_headers), dict(seen_headers)
    for name in shared:
        if name in dynamic:
            continue
        before, after = base_values[name], seen_values[name]
        if before != after and not explained(pairs, before, after):
            drifts.append(f"{label}: {name} {before!r} -> {after!r}")
    base_body = base.get("request_body") or {}
    seen_body = seen.get("request_body") or {}
    for field in ("top_level_keys", "metadata", "requestType", "userAgent", "stream"):
        if base_body.get(field) != seen_body.get(field):
            drifts.append(f"{label}: body {field} {base_body.get(field)!r} -> {seen_body.get(field)!r}")
    base_config = sorted((base_body.get("generation_config") or {}).keys())
    seen_config = sorted((seen_body.get("generation_config") or {}).keys())
    if base_config != seen_config:
        drifts.append(f"{label}: generationConfig keys {base_config} -> {seen_config}")
    return drifts


def compare_captures(baseline: list[dict], observed: list[dict], dynamic: frozenset[str],
                     pairs: list[tuple[str, str]]) -> tuple[list[str], list[str]]:
    """(drifts, notes) between two captures, request by request."""
    base_by, seen_by = {}, {}
    for record in baseline:
        base_by.setdefault(request_key(record), record)
    for record in observed:
        seen_by.setdefault(request_key(record), record)
    drifts, notes = [], []
    for key in sorted(base_by.keys() & seen_by.keys()):
        drifts.extend(compare_records(" ".join(key), base_by[key], seen_by[key], dynamic, pairs))
    for key in sorted(base_by.keys() - seen_by.keys()):
        notes.append(f"{' '.join(key)}: in the baseline, not sent this run")
    for key in sorted(seen_by.keys() - base_by.keys()):
        notes.append(f"{' '.join(key)}: sent this run, not in the baseline")
    return drifts, notes


def load_capture_file(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def baseline_path(root: Path, client: str) -> Path:
    if client == "claude":
        return root / read_pin(root, PIN["claude.gate_baseline"])
    return root / BASELINE_DIR / f"{client}.jsonl"


def upstream_notes(capture: Capture) -> list[str]:
    counts = collections.Counter(
        f"{record.get('method')} {str(record.get('path', '')).split('?')[0]} -> {record.get('status', '?')}"
        for record in capture.records)
    return [f"upstream: {line}" + (f" (x{count})" if count > 1 else "") for line, count in sorted(counts.items())]


def analyze(root: Path, name: str, capture: Capture, proxy: Capture | None = None,
            oauth: dict | None = None) -> Result:
    result = Result(name, installed_version(CLIENTS[name]), capture)
    for run in capture.runs:
        result.info.append(f"ran `{run['argv']}`: exit {run['exit']} in {run['seconds']}s"
                           + (f" — {run['tail']}" if run["tail"] else ""))
    if not capture.records:
        result.manual.append(f"nothing reached {', '.join(CLIENTS[name].hosts)}; the client did not run "
                             "or ignored the interceptor (see the run notes)")
        return result
    result.info.extend(upstream_notes(capture))

    for pin in PINS:
        if pin.client != name or pin.observe is None or pin.name == "agy.ja4":
            continue
        try:
            current = read_pin(root, pin)
        except PinError as error:
            result.manual.append(f"pin registry out of step: {error}")
            continue
        observed = pin.observe(capture)
        if observed is None:
            result.manual.append(f"`{pin.name}` not found in the capture: the wire format may have changed")
        elif observed != current:
            result.pin_changes.append((pin, current, observed))

    for hello in HELLOS:
        if hello.client != name:
            continue
        record = hello.select(capture)
        seen = (record or {}).get("tls")
        if not seen:
            result.manual.append(f"no `{hello.name}` connection was captured, so its ClientHello is unchecked")
            continue
        pinned = tls_hello.fingerprints((root / hello.path).read_bytes())
        if seen["ja4"] == pinned["ja4"]:
            continue
        raw = capture.hellos.get(seen["ja4"])
        if raw is None:
            result.manual.append(f"`{hello.name}` JA4 changed to {seen['ja4']} but the hello bytes were not saved")
            continue
        result.hello_changes.append((hello, pinned, seen, raw))
        if hello.name.startswith("claude."):
            result.info.append(f"`{hello.name}`: the *_resumed fingerprints in the TLS reference were not re-captured")

    pairs = substitutions(result.pin_changes)
    if name == "agy":
        analyze_agy(root, result, capture, proxy, oauth, pairs)
    elif name == "claude":
        analyze_claude(root, result, capture, pairs)
    else:
        analyze_generic(root, result, capture, pairs)
    return result


def analyze_agy(root: Path, result: Result, capture: Capture, proxy: Capture | None,
                oauth: dict | None, pairs: list[tuple[str, str]]) -> None:
    agy = agy_ja4(capture)
    try:
        pinned = read_pin(root, PIN["agy.ja4"])
    except PinError as error:
        result.manual.append(f"pin registry out of step: {error}")
        pinned = None
    if proxy is None or not proxy.records:
        result.manual.append("this build's Cloud Code requests were not captured (tools/clientprobe); "
                             "JA4 parity and request shape are unchecked")
    else:
        mine = agy_ja4(proxy)
        if agy and mine != agy:
            result.manual.append(f"JA4: agy sends {agy}, this build sends {mine}. The Go toolchain decides "
                                 "the hello (AGENTS.md); move go.mod's toolchain to agy's Go release")
        elif agy and pinned and agy != pinned:
            result.pin_changes.append((PIN["agy.ja4"], pinned, agy))
        # The probe ran this tree before its pins move, so a difference the
        # pending pin changes explain (the User-Agent) is not a finding.
        pending = [(new, old) for old, new in substitutions(result.pin_changes)]
        drifts, _ = compare_captures(capture.records, proxy.records, CLIENTS["agy"].dynamic_headers, pending)
        result.manual.extend(f"this build differs from agy: {drift}" for drift in drifts)
    analyze_generic(root, result, capture, pairs)
    if oauth is None:
        return
    if oauth.get("error") and "accepted" not in oauth:
        result.manual.append(f"WebUI sign-in check did not run: {oauth['error']}")
    elif not oauth.get("accepted"):
        result.manual.append(f"WebUI sign-in: Google rejects client {oauth.get('client_id_prefix')}-… with "
                             f"`{oauth.get('error')}` for {oauth.get('redirect_uri')}; see "
                             "agyOAuthCredentialCandidates")
    else:
        result.info.append(f"WebUI sign-in: Google accepts client {oauth.get('client_id_prefix')}-… "
                           f"with {oauth.get('redirect_uri')}")


def analyze_claude(root: Path, result: Result, capture: Capture, pairs: list[tuple[str, str]]) -> None:
    observed = claude_messages(capture)
    if observed is None:
        result.manual.append("no POST /v1/messages was captured, so headers, body and cch are unchecked")
        return
    try:
        baseline = ccdiff.load_messages_records(str(baseline_path(root, "claude")))
    except (PinError, OSError) as error:
        result.manual.append(f"Claude baseline unreadable: {error}")
        baseline = []
    if not baseline:
        result.write_baseline = not result.manual
        return
    expected = {}
    for record in baseline:
        for key, pair in ccdiff.header_map(record).items():
            expected.setdefault(key, pair)
    seen = ccdiff.header_map(observed)
    accounted = {wire for key, (wire, value) in expected.items()
                 if key in seen and seen[key][1] != value and explained(pairs, value, seen[key][1])}
    drifts = [drift for drift in ccdiff.compare_headers(baseline, observed)
              if not any(drift.startswith(f"header value changed: {name}\n") for name in accounted)]
    if observed.get("path") != baseline[0].get("path"):
        drifts.append(f"request path {baseline[0].get('path')!r} -> {observed.get('path')!r}")
    drifts += ccdiff.compare_order(baseline[0], observed)
    drifts += ccdiff.compare_body(baseline, observed)
    drifts += ccdiff.compare_correlations(observed)
    # cch and the cc_version suffix, recomputed with the pinned seed and salt
    # (scripts/cc_billing.py): a mismatch means the signature derivation moved.
    drifts += ccdiff.compare_billing(observed)
    result.manual.extend(drift.replace("\n", " ") for drift in drifts)
    result.write_baseline = bool(result.pin_changes or result.hello_changes) and not result.manual


def analyze_generic(root: Path, result: Result, capture: Capture, pairs: list[tuple[str, str]]) -> None:
    path = baseline_path(root, result.client)
    if not path.exists():
        result.info.append(f"no baseline yet; this run records {path.relative_to(root)}")
        result.write_baseline = True
        return
    drifts, notes = compare_captures(load_capture_file(path), capture.records,
                                     CLIENTS[result.client].dynamic_headers, pairs)
    result.manual.extend(f"changed since the baseline: {drift}" for drift in drifts)
    result.info.extend(notes)
    result.write_baseline = bool(result.pin_changes or result.hello_changes) and not drifts


# ---------------------------------------------------------------- update


def baseline_records(capture: Capture) -> list[dict]:
    """Capture records minus what is per-run or per-account (response headers carry org IDs)."""
    dropped = {"ts", "response_headers", "response_body"}
    return [{key: value for key, value in record.items() if key not in dropped} for record in capture.records]


def apply_updates(root: Path, results: list[Result]) -> list[str]:
    """Write every automatic change into root; returns the changed paths."""
    changed: set[str] = set()
    for result in results:
        for pin, _, new in result.pin_changes:
            changed.update(write_pin(root, pin, new))
        for hello, _, seen, raw in result.hello_changes:
            (root / hello.path).write_bytes(raw)
            changed.add(hello.path)
            for pin_name, key in ((hello.ja4_pin, "ja4"), (hello.ja3_pin, "ja3")):
                pin = PIN[pin_name]
                if read_pin(root, pin) != seen[key]:
                    changed.update(write_pin(root, pin, seen[key]))
        if result.write_baseline and result.capture and result.capture.records:
            target = root / BASELINE_DIR / f"{result.client}.jsonl"
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("".join(json.dumps(record, ensure_ascii=False) + "\n"
                                      for record in baseline_records(result.capture)), encoding="utf-8")
            changed.add(str(target.relative_to(root)))
            if result.client == "claude":
                relative = f"{BASELINE_DIR}/claude.jsonl"
                if read_pin(root, PIN["claude.gate_baseline"]) != relative:
                    changed.update(write_pin(root, PIN["claude.gate_baseline"], relative))
    go_files = [str(root / path) for path in changed if path.endswith(".go")]
    if go_files:
        subprocess.run(["gofmt", "-w", *go_files], check=False)
    return sorted(changed)


# The identity tooling's own tests; the scripts directory also holds
# classifier tooling whose tests need pytest and say nothing about the wire.
IDENTITY_TESTS = ("test_client_drift", "test_tls_hello", "test_cc_billing", "test_diff_claude_code_identity",
                  "test_check_identity_policy", "test_mitm_header_dump")


def verify(root: Path, results: list[Result]) -> list[tuple[str, str, str]]:
    steps = [
        ("go build ./...", ["go", "build", "./..."], root, 600),
        ("go test ./...", ["go", "test", "./..."], root, 1800),
        ("identity script tests", [sys.executable, "-m", "unittest", *IDENTITY_TESTS], root / "scripts", 600),
    ]
    claude = next((r for r in results if r.client == "claude"), None)
    if claude and claude.write_baseline:
        steps.append(("scripts/verify-claude-code-identity.sh", ["scripts/verify-claude-code-identity.sh"], root, 900))
    outcomes = []
    for label, argv, cwd, timeout in steps:
        log(f"verify: {label}")
        done = run_command(argv, dict(os.environ), cwd, timeout=timeout)
        if done["exit"] == 0:
            status = "skipped" if "SKIPPED" in done["tail"] else "pass"
        else:
            status = "fail"
        outcomes.append((label, status, done["tail"]))
    return outcomes


# ---------------------------------------------------------------- report


def fmt(value: object) -> str:
    if isinstance(value, list):
        return f"{len(value)} entries"
    return f"`{value}`"


def render_report(results: list[Result], checks: list[tuple[str, str, str]] | None, changed: list[str]) -> str:
    today = datetime.date.today().isoformat()
    lines = [f"## Client drift, {today}", "",
             "Opened by `scripts/client_drift.py` after an installed client changed. Each real client ran "
             "against its real upstream through mitmdump (credentials redacted, bodies fingerprinted).", "",
             "| Client | Installed | Requests captured | Updated automatically | Needs a human |",
             "|---|---|---|---|---|"]
    for r in results:
        auto = [f"{len(r.pin_changes)} pin(s)"] if r.pin_changes else []
        auto += [f"{len(r.hello_changes)} hello(s)"] if r.hello_changes else []
        auto += ["baseline"] if r.write_baseline else []
        lines.append(f"| {r.client} | {r.installed or 'not found'} | {len(r.capture.records) if r.capture else 0} "
                     f"| {', '.join(auto) or 'none'} | {len(r.manual)} |")
    for r in results:
        lines += ["", f"### {r.client} {r.installed or ''}".rstrip()]
        if r.pin_changes or r.hello_changes or r.write_baseline:
            lines += ["", "**Updated**", ""]
            for pin, old, new in r.pin_changes:
                if isinstance(old, list):
                    added = [b for b in new if b not in old]
                    removed = [b for b in old if b not in new]
                    detail = f"added {added}, removed {removed}" if added or removed else "reordered"
                    lines.append(f"- `{pin.name}`: {detail}")
                else:
                    lines.append(f"- `{pin.name}`: {fmt(old)} → {fmt(new)}")
            for hello, old, new, _ in r.hello_changes:
                lines.append(f"- `{hello.path}`: JA4 `{old['ja4']}` → `{new['ja4']}`, "
                             f"JA3 `{old['ja3']}` → `{new['ja3']}`")
            if r.write_baseline:
                lines.append(f"- baseline `{BASELINE_DIR}/{r.client}.jsonl`")
        if r.manual:
            lines += ["", "**Needs a human**", ""] + [f"- {item}" for item in r.manual]
        if r.info:
            lines += ["", "<details><summary>Notes</summary>", ""] + [f"- {item}" for item in r.info]
            lines += ["", "</details>"]
    if checks is not None:
        lines += ["", "### Verification", "", "| Step | Result |", "|---|---|"]
        for label, status, tail in checks:
            lines.append(f"| `{label}` | {status}{' — ' + tail.replace('|', '/') if status == 'fail' else ''} |")
    if changed:
        lines += ["", "<details><summary>Files</summary>", ""] + [f"- `{path}`" for path in changed]
        lines += ["", "</details>"]
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------- git


def git(root: Path, *args: str, check: bool = True) -> str:
    done = subprocess.run(["git", *args], cwd=root, capture_output=True, text=True)
    if check and done.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)}: {done.stderr.strip() or done.stdout.strip()}")
    return done.stdout.strip()


def fork_slug(root: Path) -> str:
    remote = git(root, "remote", "get-url", REMOTE)
    match = re.search(r"github\.com[:/](.+?)(?:\.git)?$", remote)
    if not match:
        raise RuntimeError(f"remote {REMOTE} is not a GitHub URL: {remote}")
    return match.group(1)


def prepare_worktree() -> Path:
    tree = state_dir() / "worktree"
    git(REPO, "fetch", REMOTE, BASE_BRANCH)
    if tree.exists():
        git(REPO, "worktree", "remove", "--force", str(tree), check=False)
        shutil.rmtree(tree, ignore_errors=True)
    git(REPO, "worktree", "prune")
    git(REPO, "worktree", "add", "--force", "-B", DRIFT_BRANCH, str(tree), f"{REMOTE}/{BASE_BRANCH}")
    return tree


def publish(root: Path, results: list[Result], changed: list[str], report: str, draft: bool) -> str:
    git(root, "add", "--", *changed)
    scan = subprocess.run([str(root / "scripts" / "secret-scan.sh"), "staged"], cwd=root,
                          capture_output=True, text=True)
    if scan.returncode != 0:
        raise RuntimeError(f"secret scan blocked the commit: {(scan.stdout + scan.stderr).strip()[-400:]}")
    moved = [f"{r.client} {r.installed}" for r in results if r.pin_changes or r.hello_changes]
    title = ("chore(identity): follow " + ", ".join(moved)) if moved else "chore(identity): refresh client drift baselines"
    git(root, "commit", "-m", title, "-m", "Generated by scripts/client_drift.py; the PR body has the report.")
    git(root, "push", "--force", REMOTE, f"HEAD:refs/heads/{DRIFT_BRANCH}")
    slug = fork_slug(root)
    body = root.parent / "pr-body.md"
    body.write_text(report, encoding="utf-8")
    listed = subprocess.run(["gh", "pr", "list", "--repo", slug, "--head", DRIFT_BRANCH, "--state", "open",
                             "--json", "number,url"], capture_output=True, text=True, check=True)
    existing = json.loads(listed.stdout or "[]")
    if existing:
        number, url = str(existing[0]["number"]), existing[0]["url"]
        # The branch was rebuilt from BASE_BRANCH, so the PR must target it too.
        subprocess.run(["gh", "pr", "edit", number, "--repo", slug, "--base", BASE_BRANCH, "--title", title,
                        "--body-file", str(body)], check=True, capture_output=True)
        subprocess.run(["gh", "pr", "ready", number, "--repo", slug, *(["--undo"] if draft else [])],
                       capture_output=True)
        return url
    argv = ["gh", "pr", "create", "--repo", slug, "--base", BASE_BRANCH, "--head", DRIFT_BRANCH,
            "--title", title, "--body-file", str(body)]
    created = subprocess.run(argv + (["--draft"] if draft else []), capture_output=True, text=True, check=True)
    return created.stdout.strip().splitlines()[-1]


# ---------------------------------------------------------------- state


def load_state() -> dict:
    try:
        return json.loads((state_dir() / "state.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def save_state(state: dict) -> None:
    path = state_dir() / "state.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(state, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def lock_file():
    path = state_dir() / "run.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    return open(path, "w")


def lock_held() -> bool:
    with lock_file() as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return True
        fcntl.flock(handle, fcntl.LOCK_UN)
        return False


def status() -> dict:
    state = load_state()
    current = signature()
    previous = state.get("signature") or {}
    changed = [name for name in CLIENTS if current.get(name) != previous.get(name)]
    due, reason = bool(changed), ("changed: " + ", ".join(changed)) if changed else "no client changed"
    last = state.get("last") or {}
    if not due and last.get("status") in ("error", "local"):
        finished = last.get("finished_epoch", 0)
        if time.time() - finished > RETRY_AFTER_FAILURE:
            due, reason = True, f"retrying after {last.get('status')}"
    if os.environ.get("CLIENT_DRIFT_DISABLED") == "1":
        due, reason = False, "CLIENT_DRIFT_DISABLED=1"
    return {"due": due, "reason": reason, "running": lock_held(), "changed": changed, "last": last,
            "log": str(state_dir() / "run.log")}


def notify(message: str) -> None:
    if sys.platform == "darwin":
        script = f"display notification {json.dumps(message)} with title \"antigravity-proxy\""
        subprocess.run(["osascript", "-e", script], capture_output=True)
    elif shutil.which("notify-send"):
        subprocess.run(["notify-send", "antigravity-proxy", message], capture_output=True)


# ---------------------------------------------------------------- commands


def collect(root: Path, run_dir: Path, clients: list[str]) -> list[Result]:
    results = []
    proxy = oauth = None
    if "agy" in clients:
        log("capture: this build's Cloud Code requests (tools/clientprobe)")
        proxy = capture_proxy_cloudcode(run_dir, root)
        log("probe: WebUI sign-in client")
        oauth = probe_oauth(root)
    for name in clients:
        log(f"capture: {name}")
        capture = capture_client(name, run_dir, root)
        results.append(analyze(root, name, capture, proxy if name == "agy" else None,
                               oauth if name == "agy" else None))
    return results


def command_check(args: argparse.Namespace) -> int:
    run_dir = Path(tempfile.mkdtemp(prefix="client-drift-check-"))
    results = collect(REPO, run_dir, args.clients)
    print(render_report(results, None, []))
    print(f"captures: {run_dir}", file=sys.stderr)
    return 1 if any(r.manual or r.changes for r in results) else 0


def command_update(args: argparse.Namespace) -> int:
    """Runs inside the drift worktree; writes the outcome to --result."""
    root = Path(args.root).resolve()
    run_dir = Path(args.run_dir)
    outcome: dict = {"status": "error", "summary": "", "pr_url": None}
    try:
        results = collect(root, run_dir, list(CLIENTS))
        # A client that ran and recorded nothing failed for a reason this run
        # cannot judge (a login prompt, a network fault). Publishing the rest
        # would leave it unchecked until the next update, so the run fails and
        # is retried after the backoff instead. A client not installed, or
        # skipped for a missing credential, does not block the others.
        silent = [r.client for r in results if r.capture and not r.capture.records
                  and any(run["exit"] not in ("missing", "skipped") for run in r.capture.runs)]
        if silent:
            report = render_report(results, None, [])
            (run_dir / "report.md").write_text(report, encoding="utf-8")
            outcome.update(status="error", summary=f"no capture from {', '.join(silent)}; retrying later",
                           report=str(run_dir / "report.md"))
            Path(args.result).write_text(json.dumps(outcome), encoding="utf-8")
            return 0
        manual = sum(len(r.manual) for r in results)
        moved = [f"{r.client} {r.installed}" for r in results if r.pin_changes or r.hello_changes]
        if not any(r.changes for r in results) and not manual:
            report = render_report(results, None, [])
            outcome.update(status="clean", summary="all three clients match the proxy's pins")
        else:
            changed = apply_updates(root, results)
            checks = verify(root, results) if changed else []
            report = render_report(results, checks, changed)
            failed = [label for label, state, _ in checks if state == "fail"]
            summary = ", ".join(moved) if moved else "no pin moved"
            summary += f"; {manual} manual item(s)" if manual else ""
            summary += f"; failing: {', '.join(failed)}" if failed else ""
            if not changed:
                outcome.update(status="manual", summary=summary + " (nothing to commit)")
            else:
                try:
                    url = publish(root, results, changed, report, draft=bool(manual or failed))
                    outcome.update(status="pr", summary=summary, pr_url=url)
                except (RuntimeError, subprocess.CalledProcessError) as error:
                    detail = getattr(error, "stderr", None) or str(error)
                    outcome.update(status="local", summary=f"{summary}; not published: {str(detail).strip()[-300:]}")
        (run_dir / "report.md").write_text(report, encoding="utf-8")
        outcome["report"] = str(run_dir / "report.md")
    except Exception as error:  # noqa: BLE001 - every failure must reach the state file
        outcome.update(status="error", summary=f"{type(error).__name__}: {error}")
    Path(args.result).write_text(json.dumps(outcome), encoding="utf-8")
    return 0


def command_run(_args: argparse.Namespace) -> int:
    handle = lock_file()
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        log("another run holds the lock; exiting")
        return 0
    started = signature()
    run_dir = state_dir() / "runs" / datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    run_dir.mkdir(parents=True, exist_ok=True)
    result_path = run_dir / "result.json"
    outcome = {"status": "error", "summary": "the update step produced no result"}
    try:
        tree = prepare_worktree()
        log(f"worktree {tree} at {REMOTE}/{BASE_BRANCH}")
        # The pins registry describes the tree it ships in, so the worktree's
        # own copy runs when it has one.
        script = tree / "scripts" / "client_drift.py"
        if not script.exists():
            script = Path(__file__).resolve()
        subprocess.run([sys.executable, str(script), "_update", "--root", str(tree),
                        "--run-dir", str(run_dir), "--result", str(result_path)], check=False)
        if result_path.exists():
            outcome = json.loads(result_path.read_text(encoding="utf-8"))
    except Exception as error:  # noqa: BLE001
        outcome = {"status": "error", "summary": f"{type(error).__name__}: {error}"}
    state = load_state()
    state["signature"] = started
    state["last"] = dict(outcome, finished_at=datetime.datetime.now().isoformat(timespec="seconds"),
                         finished_epoch=int(time.time()), reported=False, run_dir=str(run_dir))
    save_state(state)
    message = f"client drift: {outcome['status']}: {outcome.get('summary', '')}"
    if outcome.get("pr_url"):
        message += f" {outcome['pr_url']}"
    log(message)
    notify(message)
    return 0


def command_status(args: argparse.Namespace) -> int:
    current = status()
    if args.json:
        print(json.dumps(current))
    else:
        print(f"due: {current['due']} ({current['reason']}); running: {current['running']}")
        if current["last"]:
            last = current["last"]
            print(f"last: {last.get('status')} at {last.get('finished_at')}: {last.get('summary')} "
                  f"{last.get('pr_url') or ''}".rstrip())
    return 0


def command_ack(_args: argparse.Namespace) -> int:
    state = load_state()
    if state.get("last"):
        state["last"]["reported"] = True
        save_state(state)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)
    status_parser = commands.add_parser("status")
    status_parser.add_argument("--json", action="store_true")
    check_parser = commands.add_parser("check")
    check_parser.add_argument("--clients", type=lambda text: text.split(","), default=list(CLIENTS))
    commands.add_parser("run")
    commands.add_parser("ack")
    update_parser = commands.add_parser("_update")
    update_parser.add_argument("--root", required=True)
    update_parser.add_argument("--run-dir", required=True)
    update_parser.add_argument("--result", required=True)
    args = parser.parse_args()
    return {"status": command_status, "check": command_check, "run": command_run,
            "ack": command_ack, "_update": command_update}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
