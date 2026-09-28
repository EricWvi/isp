#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/home"

cat >"$test_dir/bin/id" <<'SH'
#!/usr/bin/env bash
case "$1" in
  -u) echo 1001 ;;
  -un) echo tester ;;
esac
SH
cat >"$test_dir/bin/systemctl" <<'SH'
#!/usr/bin/env bash
case "$*" in
  '--user show-environment')
    [[ ${XDG_RUNTIME_DIR:-} == /run/user/1001 &&
       ${DBUS_SESSION_BUS_ADDRESS:-} == unix:path=/run/user/1001/bus ]] || exit 1
    if [[ ${TEST_REQUIRE_LINGER:-} == 1 ]]; then
      [[ -f $TEST_LINGER_FILE ]]
    fi
    ;;
  '--user is-active --quiet isp-proxy.service') exit 1 ;;
  '--user is-active isp-proxy.service') echo active ;;
  '--user daemon-reload'|'--user enable --now isp-proxy.service') exit 0 ;;
  *) echo "unexpected systemctl call: $*" >&2; exit 1 ;;
esac
SH
cat >"$test_dir/bin/loginctl" <<'SH'
#!/usr/bin/env bash
case "$1" in
  show-user)
    if [[ ${TEST_REQUIRE_LINGER:-} == 1 && ! -f $TEST_LINGER_FILE ]]; then
      if [[ ${TEST_REQUIRE_SUDO:-} == 1 ]]; then
        echo 'Failed to get user: user is not logged in or lingering' >&2
        exit 1
      fi
      echo no
    else
      echo yes
    fi
    ;;
  enable-linger)
    [[ ${TEST_REQUIRE_SUDO:-} != 1 ]] || exit 1
    touch "$TEST_LINGER_FILE"
    ;;
esac
SH
cat >"$test_dir/bin/sudo" <<'SH'
#!/usr/bin/env bash
if [[ $1 == -n ]]; then
  echo 'sudo needs an interactive password' >&2
  exit 1
fi
[[ $1 == loginctl && $2 == enable-linger ]]
touch "$TEST_LINGER_FILE"
SH
cat >"$test_dir/bin/curl" <<'SH'
#!/usr/bin/env bash
if [[ " $* " == *' -w '* ]]; then
  echo -n 'https://github.com/EricWvi/isp/releases/tag/v0.1.0'
  exit 0
fi
while (($#)); do
  if [[ $1 == -o ]]; then
    shift
    output=$1
  fi
  shift
done
case "$output" in
  */isp-proxy)
    printf '#!/usr/bin/env bash\necho "configuration valid"\n' >"$output"
    ;;
  */isp-proxy.service)
    printf '[Unit]\nDescription=ISP Proxy\n' >"$output"
    ;;
esac
SH
chmod 0755 "$test_dir/bin/"*

if ! env -u XDG_RUNTIME_DIR -u DBUS_SESSION_BUS_ADDRESS \
  HOME="$test_dir/home" PATH="$test_dir/bin:$PATH" \
  bash "$repo_dir/scripts/install.sh" >"$test_dir/output" 2>&1; then
  cat "$test_dir/output" >&2
  exit 1
fi
grep -q '已安装 v0.1.0' "$test_dir/output"
grep -q 'socks5_enabled: false' "$test_dir/home/.config/isp-proxy/config.yaml"
grep -q 'http_proxy_enabled: false' "$test_dir/home/.config/isp-proxy/config.yaml"
echo 'installer test passed: missing session bus environment'

mkdir -p "$test_dir/second-home"
if ! env -u XDG_RUNTIME_DIR -u DBUS_SESSION_BUS_ADDRESS \
  HOME="$test_dir/second-home" PATH="$test_dir/bin:$PATH" \
  TEST_REQUIRE_LINGER=1 TEST_LINGER_FILE="$test_dir/linger-enabled" \
  bash "$repo_dir/scripts/install.sh" >"$test_dir/second-output" 2>&1; then
  cat "$test_dir/second-output" >&2
  exit 1
fi
grep -q '已安装 v0.1.0' "$test_dir/second-output"
[[ -f $test_dir/linger-enabled ]]
echo 'installer test passed: user manager starts after enabling linger'

mkdir -p "$test_dir/third-home"
if ! env -u XDG_RUNTIME_DIR -u DBUS_SESSION_BUS_ADDRESS \
  HOME="$test_dir/third-home" PATH="$test_dir/bin:$PATH" \
  TEST_REQUIRE_LINGER=1 TEST_REQUIRE_SUDO=1 \
  TEST_LINGER_FILE="$test_dir/sudo-linger-enabled" \
  bash "$repo_dir/scripts/install.sh" >"$test_dir/third-output" 2>&1; then
  cat "$test_dir/third-output" >&2
  exit 1
fi
grep -q '已安装 v0.1.0' "$test_dir/third-output"
[[ -f $test_dir/sudo-linger-enabled ]]
echo 'installer test passed: no user session, sudo required for linger'
