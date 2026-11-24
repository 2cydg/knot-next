#!/usr/bin/env sh
set -eu
umask 077
export LC_ALL=C

version=${KNOT_VERSION:-}
install_dir=${KNOT_INSTALL_DIR:-"$HOME/.local/bin"}
base_url=${KNOT_BASE_URL:-https://github.com/2cydg/knot-next/releases}

fail() {
  printf 'knot install: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version | --install-dir | --base-url)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "$1 requires a value"
      case "$1" in
        --version) version=$2 ;;
        --install-dir) install_dir=$2 ;;
        --base-url) base_url=$2 ;;
      esac
      shift 2
      ;;
    -h | --help)
      printf '%s\n' 'Usage: install.sh [--version TAG] [--install-dir DIR] [--base-url URL]' \
        'Environment: KNOT_VERSION, KNOT_INSTALL_DIR, KNOT_BASE_URL'
      exit 0
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

base_url=${base_url%/}
case "$version" in
  "") release_url="$base_url/latest/download" ;;
  *[!A-Za-z0-9._+-]* | . | ..) fail "invalid release tag" ;;
  *) release_url="$base_url/download/$version" ;;
esac

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported operating system" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU architecture" ;;
esac

download() {
  if command -v curl >/dev/null 2>&1; then
    curl -q -fsSL -o "$2" -- "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" -- "$1"
  else
    fail "curl or wget is required"
  fi
}

download_binary() {
  asset=$1
  destination=$2
  expected=$(awk -v asset="$asset" '
    { sub(/\r$/, "") }
    NF == 2 && ($2 == asset || $2 == "*" asset) && length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/ {
      hash = tolower($1)
      count++
    }
    END { if (count == 1) print hash; else exit 1 }
  ' "$install_stage/checksums.txt") || fail "missing or ambiguous checksum for $asset"
  printf 'Downloading %s\n' "$asset"
  download "$release_url/$asset" "$destination" || fail "failed to download $asset"
  if command -v sha256sum >/dev/null 2>&1; then
    hash_output=$(sha256sum < "$destination")
  elif command -v shasum >/dev/null 2>&1; then
    hash_output=$(shasum -a 256 < "$destination")
  else
    fail "sha256sum or shasum is required"
  fi
  [ "${hash_output%% *}" = "$expected" ] || fail "checksum mismatch for $asset"
  chmod 755 "$destination"
}

exists() {
  [ -e "$1" ] || [ -L "$1" ]
}

# Copy the TOML string expression so the secret never becomes a command argument.
write_connection() {
  KNOT_CORE_BIN="$install_dir/knot-core" KNOT_CORE_CONFIG="$core_config" awk '
    function key_name(key) {
      gsub(/^[ \t]+|[ \t]+$/, "", key)
      if (key ~ /^"[a-z_]+"$/ || key ~ /^\047[a-z_]+\047$/)
        return substr(key, 2, length(key) - 2)
      return key
    }
    function quote_path(path,    result, i, char) {
      for (i = 1; i <= length(path); i++) {
        char = substr(path, i, 1)
        if (char == "\\" || char == "\"") result = result "\\"
        result = result char
      }
      return "\"" result "\""
    }
    BEGIN {
      port = 17898
      token = "\"\""
      exe = ENVIRON["KNOT_CORE_BIN"]
      cfg = ENVIRON["KNOT_CORE_CONFIG"]
      if (exe ~ /[[:cntrl:]]/ || cfg ~ /[[:cntrl:]]/) invalid = 1
    }
    NR == 1 { sub(/^\357\273\277/, "") }
    { sub(/\r$/, "") }
    /^[ \t]*\[/ {
      table_seen = 1
      service = $0 ~ /^[ \t]*\[[ \t]*(service|"service"|\047service\047)[ \t]*\][ \t]*(#.*)?$/
      if (!service && $0 ~ /^[ \t]*\[+[ \t]*(service|"service"|\047service\047)[ \t]*(\.|\])/)
        invalid = 1
      if (service && service_count++) invalid = 1
      next
    }
    !table_seen && /^[ \t]*(service|"service"|\047service\047)[ \t]*[.=]/ { invalid = 1 }
    service && /=/ {
      equal = index($0, "=")
      key = key_name(substr($0, 1, equal - 1))
      value = substr($0, equal + 1)
      sub(/^[ \t]+/, "", value)
      if (key == "port") {
        if (port_count++) invalid = 1
        sub(/[ \t]*(#.*)?$/, "", value)
        if (value !~ /^\+?[0-9]+(_[0-9]+)*$/) invalid = 1
        gsub(/_/, "", value)
        port = value + 0
      } else if (key == "secret") {
        if (token_count++) invalid = 1
        if (value !~ /^"([^"\\]|\\.)*"[ \t]*(#.*)?$/ && value !~ /^\047[^\047]*\047[ \t]*(#.*)?$/)
          invalid = 1
        token = value
      }
    }
    END {
      if (invalid || port < 1 || port > 65535) exit 1
      print "[core]"
      printf "url = \"http://127.0.0.1:%d\"\n", port
      print "secret = " token
      print "auto_start = true"
      print "executable = " quote_path(exe)
      print "config_file = " quote_path(cfg)
    }
  ' "$1"
}

update_client() {
  awk '
    NR == FNR { block = block $0 "\n"; next }
    FNR == 1 { sub(/^\357\273\277/, "") }
    { sub(/\r$/, "") }
    /^[ \t]*\[[ \t]*(core|"core"|\047core\047)[ \t]*\][ \t]*(#.*)?$/ {
      table_seen = 1
      if (found++) invalid = 1
      core = 1
      printf "%s", block
      next
    }
    /^[ \t]*\[/ {
      table_seen = 1
      if ($0 ~ /^[ \t]*\[+[ \t]*(core|"core"|\047core\047)[ \t]*(\.|\])/) invalid = 1
      core = 0
    }
    !table_seen && /^[ \t]*(core|"core"|\047core\047)[ \t]*[.=]/ { invalid = 1 }
    core && /=/ {
      equal = index($0, "=")
      key = substr($0, 1, equal - 1)
      gsub(/^[ \t]+|[ \t]+$/, "", key)
      if (key ~ /^"[a-z_]+"$/ || key ~ /^\047[a-z_]+\047$/) key = substr(key, 2, length(key) - 2)
      if (key ~ /^(url|secret|auto_start|executable|config_file)$/) {
        value = substr($0, equal + 1)
        if (value ~ /^[ \t]*("""|\047\047\047)/) invalid = 1
        next
      }
    }
    { print }
    END {
      if (invalid) exit 1
      if (!found) printf "\n%s", block
    }
  ' "$connection" "$client_config"
}

cleanup() {
  status=$?
  trap - 0
  if [ -n "$install_stage" ]; then
    if [ "$installed" = no ]; then
      for name in knot knot-core; do
        if [ -n "$backup_dir" ] && exists "$backup_dir/$name"; then
          mv -f "$backup_dir/$name" "$install_dir/" ||
            printf 'Restore %s from %s\n' "$name" "$backup_dir" >&2
        elif [ "$replace_ready" = yes ] && [ ! -f "$install_stage/$name" ]; then
          rm -f "$install_dir/$name"
        fi
      done
      if [ -n "$backup_dir" ]; then rmdir "$backup_dir" 2>/dev/null || :; fi
    fi
    rm -rf "$install_stage"
  fi
  if [ -n "$config_stage" ]; then rm -rf "$config_stage"; fi
  exit "$status"
}

case "$install_dir" in /*) ;; *) install_dir="./$install_dir" ;; esac
mkdir -p "$install_dir"
install_dir=$(CDPATH= cd -P "$install_dir" && pwd)
install_stage=
config_stage=
backup_dir=
installed=no
replace_ready=no
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM
stage="$install_dir/.knot-install.lock"
mkdir "$stage" || fail "could not acquire install lock $stage"
install_stage=$stage

download "$release_url/checksums.txt" "$install_stage/checksums.txt" ||
  fail "could not download checksums.txt from $release_url"
download_binary "knot-cli-$os-$arch" "$install_stage/knot"
download_binary "knot-core-$os-$arch" "$install_stage/knot-core"
for name in knot knot-core; do
  target="$install_dir/$name"
  if exists "$target"; then
    [ -f "$target" ] || { [ -L "$target" ] && [ ! -d "$target" ]; } ||
      fail "expected a binary at $target"
    if [ -z "$backup_dir" ]; then
      backup_dir=$(mktemp -d "$install_dir/.knot-backup.XXXXXX")
    fi
    mv "$target" "$backup_dir/"
  fi
done
replace_ready=yes
mv "$install_stage/knot-core" "$install_dir/"
mv "$install_stage/knot" "$install_dir/"
installed=yes
printf 'Installed %s and %s\n' "$install_dir/knot" "$install_dir/knot-core"
if [ -n "$backup_dir" ]; then printf 'Backed up previous binaries to %s\n' "$backup_dir"; fi

config_dir="$HOME/.config/knot"
mkdir -p "$config_dir"
config_dir=$(CDPATH= cd -P "$config_dir" && pwd)
stage="$config_dir/.knot-setup.lock"
mkdir "$stage" || fail "could not acquire configuration lock $stage"
config_stage=$stage
core_config="$config_dir/config.toml"
client_config="$config_dir/client.toml"
for config in "$core_config" "$client_config"; do
  if exists "$config"; then [ -f "$config" ] || fail "expected a configuration file at $config"; fi
done

if [ -f "$core_config" ] && [ -f "$client_config" ]; then
  printf 'Core and client configurations already exist.\n'
else
  source_config=$core_config
  if [ ! -f "$core_config" ]; then
    (cd "$config_stage" && "$install_dir/knot-core" init) || fail "knot-core init failed"
    source_config="$config_stage/config.toml"
  fi
  connection="$config_stage/connection.toml"
  write_connection "$source_config" > "$connection" ||
    fail "could not read service settings from $core_config"
  if [ -f "$client_config" ]; then
    [ ! -L "$client_config" ] || fail "cannot update a symbolic link at $client_config"
    update_client > "$config_stage/client.toml" || fail "could not update $client_config"
    mv -f "$config_stage/client.toml" "$config_dir/"
  else
    mv "$connection" "$config_stage/client.toml"
    ln "$config_stage/client.toml" "$config_dir/"
  fi
  # Publish core last. A retry can still repair a client-only installation.
  if [ "$source_config" != "$core_config" ]; then
    ln "$source_config" "$config_dir/"
  fi
  printf 'Configured the local core connection in %s\n' "$client_config"
fi

case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH if knot is not found by your shell.\n' "$install_dir" ;;
esac
printf 'Run knot to open the client.\n'
