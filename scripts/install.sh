#!/usr/bin/env bash
set -Eeuo pipefail

repo=EricWvi/isp
service=isp-proxy.service
user_name=$(id -un)
binary_dir="$HOME/.local/bin"
config_dir="$HOME/.config/isp-proxy"
data_dir="$HOME/.local/share/isp-proxy"
unit_dir=${ISP_PROXY_SYSTEMD_DIR:-/etc/systemd/system}
binary="$binary_dir/isp-proxy"
config="$config_dir/config.yaml"
unit="$unit_dir/$service"
legacy_unit="$HOME/.config/systemd/user/$service"

if [[ $(uname -s) != Linux ]]; then
  echo '此脚本仅支持 Linux。' >&2
  exit 1
fi
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "不支持的架构: $(uname -m)" >&2; exit 1 ;;
esac
if [[ $(id -u) -eq 0 ]]; then
  echo '请用要运行服务的普通用户执行，不要使用 sudo bash。' >&2
  exit 1
fi
if [[ ! $user_name =~ ^[a-zA-Z0-9_.-]+$ || ! $HOME =~ ^/[a-zA-Z0-9_./-]+$ ]]; then
  echo '用户名或 HOME 路径包含服务模板不支持的字符。' >&2
  exit 1
fi
for command_name in curl sudo systemctl install mktemp; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "缺少命令: $command_name" >&2
    exit 1
  fi
done

version=${ISP_PROXY_VERSION:-}
if [[ -z $version ]]; then
  latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")
  version=${latest_url##*/}
fi
if [[ ! $version =~ ^v[[:alnum:]._-]+$ ]]; then
  echo "无法确定有效的发布版本: $version" >&2
  exit 1
fi
echo "准备安装 $version (linux/$arch)"

temp_dir=$(mktemp -d)
deployment_started=false
was_active=false
had_binary=false
had_unit=false
had_legacy=false
legacy_active=false
legacy_enabled=false
rollback() {
  echo '安装失败，正在恢复原有服务文件。' >&2
  sudo systemctl stop "$service" || true
  if [[ $had_binary == true ]]; then
    install -m 0755 "$temp_dir/old-binary" "$binary" || true
  elif [[ -e $binary ]]; then
    rm -- "$binary" || true
  fi
  if [[ $had_unit == true ]]; then
    sudo install -m 0644 "$temp_dir/old-unit" "$unit" || true
  else
    sudo systemctl disable "$service" || true
    sudo rm -f -- "$unit" || true
  fi
  sudo systemctl daemon-reload || true
  if [[ $was_active == true ]]; then
    sudo systemctl start "$service" || true
  fi
  if [[ $had_legacy == true ]]; then
    if [[ ! -f $legacy_unit ]]; then
      install -m 0644 "$temp_dir/old-legacy-unit" "$legacy_unit" || true
    fi
    if [[ $legacy_enabled == true ]]; then
      systemctl --user enable "$service" || true
    fi
    if [[ $legacy_active == true ]]; then
      systemctl --user start "$service" || true
    fi
  fi
}
cleanup() {
  status=$?
  trap - EXIT
  if (( status != 0 )) && [[ $deployment_started == true ]]; then
    rollback
  fi
  rm -rf -- "$temp_dir"
  exit "$status"
}
trap cleanup EXIT

curl -fL --retry 3 "https://github.com/$repo/releases/download/$version/isp-proxy-linux-$arch" \
  -o "$temp_dir/isp-proxy"
curl -fL --retry 3 "https://raw.githubusercontent.com/$repo/main/packaging/isp-proxy.service.in" \
  -o "$temp_dir/isp-proxy.service.in"
chmod 0755 "$temp_dir/isp-proxy"
unit_text=$(<"$temp_dir/isp-proxy.service.in")
unit_text=${unit_text//@USER@/$user_name}
unit_text=${unit_text//@HOME@/$HOME}
printf '%s\n' "$unit_text" >"$temp_dir/$service"

cat >"$temp_dir/config.yaml" <<'YAML'
server:
  http_listen: 127.0.0.1:38080
  socks5_enabled: false
  http_proxy_enabled: false
  database: ./data/isp.db
providers: []
YAML

mkdir -p "$binary_dir" "$config_dir" "$data_dir/data"
if [[ -f $config ]]; then
  config_to_check=$config
else
  config_to_check="$temp_dir/config.yaml"
fi
"$temp_dir/isp-proxy" config-check "$config_to_check"
sudo -v

if [[ -f $binary ]]; then
  cp -p "$binary" "$temp_dir/old-binary"
  had_binary=true
fi
if sudo test -f "$unit"; then
  sudo cat "$unit" >"$temp_dir/old-unit"
  if ! grep -Fxq "User=$user_name" "$temp_dir/old-unit"; then
    echo "$unit 已由其他用户或方式管理，停止安装以免覆盖。" >&2
    exit 1
  fi
  had_unit=true
fi
if [[ -f $legacy_unit ]]; then
  if ! systemctl --user show-environment >/dev/null 2>&1; then
    echo "检测到旧用户服务 $legacy_unit，但无法连接用户管理器；请先手动停用旧服务。" >&2
    exit 1
  fi
  had_legacy=true
  cp -p "$legacy_unit" "$temp_dir/old-legacy-unit"
  if systemctl --user is-active --quiet "$service"; then
    legacy_active=true
  fi
  if systemctl --user is-enabled --quiet "$service"; then
    legacy_enabled=true
  fi
fi
if sudo systemctl is-active --quiet "$service"; then
  was_active=true
fi
if [[ $had_unit == false && $was_active == true ]]; then
  echo "检测到其他来源的运行中系统服务 $service，停止安装以免覆盖。" >&2
  exit 1
fi
deployment_started=true
if [[ $had_legacy == true ]]; then
  systemctl --user disable --now "$service"
fi
if [[ $was_active == true ]]; then
  sudo systemctl stop "$service"
fi

if [[ ! -f $config ]]; then
  install -m 0600 "$temp_dir/config.yaml" "$config"
  echo "已创建配置: $config"
else
  echo "保留已有配置: $config"
fi
install -m 0755 "$temp_dir/isp-proxy" "$binary"
sudo install -m 0644 "$temp_dir/$service" "$unit"
sudo systemctl daemon-reload
if [[ $had_unit == false ]]; then
  sudo systemctl enable --now "$service"
elif [[ $was_active == true ]]; then
  sudo systemctl start "$service"
fi
if [[ $had_legacy == true ]]; then
  rm -- "$legacy_unit"
  systemctl --user daemon-reload
fi
deployment_started=false

echo "已安装 $version。服务状态: $(sudo systemctl is-active "$service" || true)"
echo "配置文件: $config"
echo "查看日志: sudo journalctl -u $service -f"
