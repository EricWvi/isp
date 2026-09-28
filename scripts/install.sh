#!/usr/bin/env bash
set -Eeuo pipefail

repo=EricWvi/isp
user_name=$(id -un)
binary_dir="$HOME/.local/bin"
config_dir="$HOME/.config/isp-proxy"
unit_dir="$HOME/.config/systemd/user"
data_dir="$HOME/.local/share/isp-proxy"
binary="$binary_dir/isp-proxy"
config="$config_dir/config.yaml"
unit="$unit_dir/isp-proxy.service"
service=isp-proxy.service

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
for command_name in curl systemctl loginctl install mktemp; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "缺少命令: $command_name" >&2
    exit 1
  fi
done
if ! systemctl --user show-environment >/dev/null 2>&1; then
  echo '无法连接 systemd 用户管理器，请先以目标用户登录后重试。' >&2
  exit 1
fi

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
rollback() {
  echo '安装失败，正在恢复原有服务文件。' >&2
  if [[ $had_unit == false ]]; then
    systemctl --user disable "$service" || true
  fi
  if [[ $had_binary == true ]]; then
    install -m 0755 "$temp_dir/old-binary" "$binary" || true
  elif [[ -e $binary ]]; then
    rm -- "$binary" || true
  fi
  if [[ $had_unit == true ]]; then
    install -m 0644 "$temp_dir/old-unit" "$unit" || true
  elif [[ -e $unit ]]; then
    rm -- "$unit" || true
  fi
  systemctl --user daemon-reload || true
  if [[ $was_active == true ]]; then
    systemctl --user start "$service" || true
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
curl -fL --retry 3 "https://raw.githubusercontent.com/$repo/$version/packaging/isp-proxy.service.in" \
  -o "$temp_dir/isp-proxy.service"
chmod 0755 "$temp_dir/isp-proxy"

cat >"$temp_dir/config.yaml" <<'YAML'
server:
  http_listen: 127.0.0.1:8080
  socks5_enabled: false
  http_proxy_enabled: false
  database: ./data/isp.db
providers: []
YAML

mkdir -p "$binary_dir" "$config_dir" "$unit_dir" "$data_dir/data"
if [[ -f $config ]]; then
  config_to_check=$config
else
  config_to_check="$temp_dir/config.yaml"
fi
"$temp_dir/isp-proxy" config-check "$config_to_check"

if [[ -f $binary ]]; then
  cp -p "$binary" "$temp_dir/old-binary"
  had_binary=true
fi
if [[ -f $unit ]]; then
  cp -p "$unit" "$temp_dir/old-unit"
  had_unit=true
fi
if systemctl --user is-active --quiet "$service"; then
  was_active=true
  systemctl --user stop "$service"
fi
deployment_started=true

if [[ ! -f $config ]]; then
  install -m 0600 "$temp_dir/config.yaml" "$config"
  echo "已创建配置: $config"
else
  echo "保留已有配置: $config"
fi
install -m 0755 "$temp_dir/isp-proxy" "$binary"
install -m 0644 "$temp_dir/isp-proxy.service" "$unit"
systemctl --user daemon-reload
if [[ $had_unit == false ]]; then
  systemctl --user enable --now "$service"
elif [[ $was_active == true ]]; then
  systemctl --user start "$service"
fi
deployment_started=false

if [[ $(loginctl show-user "$user_name" -p Linger --value 2>/dev/null || true) != yes ]]; then
  if command -v sudo >/dev/null 2>&1; then
    if [[ -t 2 ]]; then
      sudo loginctl enable-linger "$user_name" || true
    else
      sudo -n loginctl enable-linger "$user_name" 2>/dev/null || true
    fi
  fi
  if [[ $(loginctl show-user "$user_name" -p Linger --value 2>/dev/null || true) != yes ]]; then
    echo "要在未登录时开机启动，请执行: sudo loginctl enable-linger $user_name" >&2
  fi
fi

echo "已安装 $version。服务状态: $(systemctl --user is-active "$service" || true)"
echo "配置文件: $config"
echo "查看日志: journalctl --user -u $service -f"
