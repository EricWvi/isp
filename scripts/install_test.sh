#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/home" "$test_dir/systemd"

cat >"$test_dir/bin/id" <<'SH'
#!/usr/bin/env bash
case "$1" in
  -u) echo 1001 ;;
  -un) echo tester ;;
esac
SH
cat >"$test_dir/bin/sudo" <<'SH'
#!/usr/bin/env bash
[[ $1 == -v ]] && exit 0
"$@"
SH
cat >"$test_dir/bin/systemctl" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$TEST_LOG"
case "$*" in
  'is-active --quiet isp-proxy.service') [[ $(cat "$TEST_STATE" 2>/dev/null || true) == active ]] ;;
  'is-active isp-proxy.service') cat "$TEST_STATE"; [[ $(cat "$TEST_STATE") == active ]] ;;
  'stop isp-proxy.service'|'disable isp-proxy.service') echo inactive >"$TEST_STATE" ;;
  'start isp-proxy.service'|'enable --now isp-proxy.service')
    if [[ ${TEST_FAIL_START:-} == 1 && ! -f $TEST_FAILED_ONCE ]]; then
      touch "$TEST_FAILED_ONCE"
      exit 1
    fi
    echo active >"$TEST_STATE"
    ;;
  'daemon-reload') exit 0 ;;
  '--user show-environment') exit 0 ;;
  '--user is-active --quiet isp-proxy.service'|'--user is-enabled --quiet isp-proxy.service') exit 0 ;;
  '--user disable --now isp-proxy.service'|'--user daemon-reload') exit 0 ;;
  *) echo "unexpected systemctl call: $*" >&2; exit 1 ;;
esac
SH
cat >"$test_dir/bin/curl" <<'SH'
#!/usr/bin/env bash
if [[ " $* " == *' -w '* ]]; then
  printf 'https://github.com/EricWvi/isp/releases/tag/v0.1.0'
  exit 0
fi
[[ ${TEST_FAIL_DOWNLOAD:-} != 1 ]] || exit 22
while (($#)); do
  if [[ $1 == -o ]]; then
    shift
    output=$1
  fi
  shift
done
case "$output" in
  */isp-proxy)
    printf '#!/usr/bin/env bash\n# %s\necho "configuration valid"\n' "${TEST_BIN_VERSION:-v1}" >"$output"
    ;;
  */isp-proxy.service.in) cp "$TEST_UNIT_TEMPLATE" "$output" ;;
  *) exit 1 ;;
esac
SH
chmod 0755 "$test_dir/bin/"*

export HOME="$test_dir/home"
export PATH="$test_dir/bin:$PATH"
export ISP_PROXY_SYSTEMD_DIR="$test_dir/systemd"
export TEST_STATE="$test_dir/state"
export TEST_LOG="$test_dir/log"
export TEST_FAILED_ONCE="$test_dir/failed-once"
export TEST_UNIT_TEMPLATE="$repo_dir/packaging/isp-proxy.service.in"
run_install() {
  if ! bash "$repo_dir/scripts/install.sh" >"$test_dir/output" 2>&1; then
    cat "$test_dir/output" >&2
    return 1
  fi
}

run_install
config="$HOME/.config/isp-proxy/config.yaml"
binary="$HOME/.local/bin/isp-proxy"
unit="$ISP_PROXY_SYSTEMD_DIR/isp-proxy.service"
grep -q 'socks5_enabled: false' "$config"
grep -q 'http_proxy_enabled: false' "$config"
grep -q 'http_listen: 127.0.0.1:38080' "$config"
grep -Fxq 'User=tester' "$unit"
grep -Fxq "WorkingDirectory=$HOME/.local/share/isp-proxy" "$unit"
[[ -d $HOME/.local/share/isp-proxy/data ]]
[[ $(cat "$TEST_STATE") == active ]]
! grep -q -- '--user' "$TEST_LOG"
echo 'installer test passed: first system service install'

printf '# preserve\n' >>"$config"
sed -i 's/127.0.0.1:38080/127.0.0.1:8080/' "$config"
: >"$TEST_LOG"
TEST_BIN_VERSION=v2 run_install
grep -q '^# v2$' "$binary"
grep -q '# preserve' "$config"
grep -q 'http_listen: 127.0.0.1:8080' "$config"
grep -q '^stop isp-proxy.service$' "$TEST_LOG"
grep -q '^start isp-proxy.service$' "$TEST_LOG"
echo 'installer test passed: running service update'

echo inactive >"$TEST_STATE"
: >"$TEST_LOG"
run_install
! grep -q '^start isp-proxy.service$' "$TEST_LOG"
[[ $(cat "$TEST_STATE") == inactive ]]
echo 'installer test passed: stopped service stays stopped'

echo active >"$TEST_STATE"
: >"$TEST_LOG"
if TEST_FAIL_DOWNLOAD=1 bash "$repo_dir/scripts/install.sh" >"$test_dir/output" 2>&1; then
  echo 'download failure was unexpectedly accepted' >&2
  exit 1
fi
! grep -q '^stop isp-proxy.service$' "$TEST_LOG"
echo 'installer test passed: download failure keeps service running'

: >"$TEST_LOG"
if TEST_BIN_VERSION=v3 TEST_FAIL_START=1 bash "$repo_dir/scripts/install.sh" >"$test_dir/output" 2>&1; then
  echo 'start failure was unexpectedly accepted' >&2
  exit 1
fi
grep -q '^# v1$' "$binary"
[[ $(cat "$TEST_STATE") == active ]]
echo 'installer test passed: failed update restores old binary and service'

legacy_unit="$HOME/.config/systemd/user/isp-proxy.service"
mkdir -p "$(dirname "$legacy_unit")"
printf '[Unit]\nDescription=Old user service\n' >"$legacy_unit"
: >"$TEST_LOG"
run_install
grep -q '^--user disable --now isp-proxy.service$' "$TEST_LOG"
[[ ! -e $legacy_unit ]]
echo 'installer test passed: old user service is removed during migration'
