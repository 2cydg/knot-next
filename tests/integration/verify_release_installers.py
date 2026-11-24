import argparse
import contextlib
import functools
import http.server
import json
import os
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import tomllib
import urllib.request
from pathlib import Path


SCRIPTS = Path(__file__).resolve().parents[2] / "scripts"
WINDOWS = os.name == "nt"
SUFFIX = ".exe" if WINDOWS else ""


@contextlib.contextmanager
def serve_assets(assets, tag, root):
    for route in (Path("download") / tag, Path("latest/download")):
        shutil.copytree(assets, root / route)
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=root)
    with http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler) as server:
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            yield f"http://127.0.0.1:{server.server_port}"
        finally:
            server.shutdown()
            thread.join()


def run(args, env, timeout=30):
    result = subprocess.run(args, env=env, capture_output=True, text=True,
                            errors="replace", timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{args[0]} exited with {result.returncode}\n"
                           f"{result.stdout}{result.stderr}")
    return result.stdout + result.stderr


def assert_start(cli, env, connection, version, commit):
    version_output = run([cli, "--version"], env)
    assert version_output.startswith(f"knot {version} (commit={commit},"), version_output
    assert "dirty=false)" in version_output, version_output

    request = urllib.request.Request(connection["url"] + "/v1/version", headers={
        "Authorization": "Bearer " + connection["secret"],
    })
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        assert json.loads(run([cli, "ls", "--json"], env)) == []
        with client.open(request, timeout=5) as response:
            build = json.load(response)["data"]
        assert build["version"] == version, build
        assert build["commit"] == commit, build
        assert build["dirty"] == "false", build
    finally:
        run([cli, "exit"], env)
        address = ("127.0.0.1", int(connection["url"].rsplit(":", 1)[1]))
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(address, timeout=0.2):
                    pass
            except OSError:
                break
            time.sleep(0.05)
        else:
            raise RuntimeError("Core did not stop after knot exit")


def verify_install(home, tag, commit, base_url, state="fresh", latest=False):
    print(f"Testing {state} configuration via {'latest' if latest else tag}", flush=True)
    install_dir = home / ".local" / "bin"
    cli, core = (install_dir / (name + SUFFIX) for name in ("knot", "knot-core"))
    config_dir = home / ".config" / "knot"
    config_dir.mkdir(parents=True)
    core_config = config_dir / "config.toml"
    client_config = config_dir / "client.toml"

    env = {key: value for key, value in os.environ.items()
           if key not in {"KNOT_VERSION", "KNOT_INSTALL_DIR", "KNOT_BASE_URL"}}
    env.update(HOME=str(home), USERPROFILE=str(home))
    env["NO_PROXY"] = "127.0.0.1,localhost"
    if base_url:
        env["KNOT_BASE_URL"] = base_url

    original_core = None
    original_client = None
    if state in {"core-only", "both"}:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        secret = secrets.token_hex(16)
        original_core = f'[service]\nport = {port}\nsecret = "{secret}"\n'.encode()
        core_config.write_bytes(original_core)
    if state == "client-only":
        client_config.write_text('language = "zh"\n[core]\n'
                                 'url = "http://127.0.0.1:17899"\n'
                                 'secret = "obsolete"\nauto_start = false\n', encoding="utf-8")
    if state == "both":
        original_client = ('language = "en"\n[core]\n'
                           f'url = "http://127.0.0.1:{port}"\nsecret = "{secret}"\n'
                           'auto_start = true\n'
                           f'executable = {json.dumps(str(core), ensure_ascii=False)}\n'
                           f'config_file = {json.dumps(str(core_config), ensure_ascii=False)}\n').encode()
        client_config.write_bytes(original_client)
        install_dir.mkdir(parents=True)
        cli.write_bytes(b"old CLI")
        core.write_bytes(b"old Core")

    if WINDOWS:
        args = ["pwsh", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
                "-File", SCRIPTS / "install.ps1"]
        version_flag = "-Version"
    else:
        args = ["sh", SCRIPTS / "install.sh"]
        version_flag = "--version"
    if not latest:
        args.extend([version_flag, tag])
    output = run(args, env, timeout=90)
    print(output.strip(), flush=True)
    assert cli.is_file() and core.is_file(), "expected both installed binaries"
    if state == "both":
        backups = list(install_dir.glob(".knot-backup.*"))
        assert len(backups) == 1 and str(backups[0]) in output, "expected reported backup path"
        assert (backups[0] / cli.name).read_bytes() == b"old CLI"
        assert (backups[0] / core.name).read_bytes() == b"old Core"

    core_bytes = core_config.read_bytes()
    client_bytes = client_config.read_bytes()
    if original_core is not None:
        assert core_bytes == original_core, "installer changed existing Core config"
    if original_client is not None:
        assert client_bytes == original_client, "installer changed existing client config"
    service = tomllib.loads(core_bytes.decode())["service"]
    preferences = tomllib.loads(client_bytes.decode())
    if state == "client-only":
        assert preferences["language"] == "zh", "installer lost the client language"
    connection = preferences["core"]
    assert service["secret"] and service["secret"] == connection["secret"], "token mismatch"
    assert connection["url"] == f"http://127.0.0.1:{service['port']}"
    assert connection["auto_start"] is True
    assert Path(connection["executable"]) == core
    assert Path(connection["config_file"]) == core_config
    assert_start(cli, env, connection, tag.removeprefix("v"), commit)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("tag")
    parser.add_argument("commit")
    parser.add_argument("--assets", type=Path, help="Serve local release assets for pre-release tests")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="knot-release-") as directory:
        root = Path(directory).resolve()
        if args.assets:
            with serve_assets(args.assets, args.tag, root / "mirror") as base_url:
                for state in ("fresh", "core-only", "client-only", "both"):
                    verify_install(root / state, args.tag, args.commit, base_url, state)
                verify_install(root / "latest", args.tag, args.commit, base_url, latest=True)
        else:
            verify_install(root / "tagged", args.tag, args.commit, None)
            verify_install(root / "latest", args.tag, args.commit, None, latest=True)


if __name__ == "__main__":
    main()
