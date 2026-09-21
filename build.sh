#!/usr/bin/env bash
#
# build.sh —— 交叉编译 Linux 二进制，并打包成可直接安装为 systemd 服务的 tar.gz。
#
# 产物：dist/cookie-proxy-<版本>-linux-<架构>.tar.gz（含同名 .sha256）
# 包内结构：
#   cookie-proxy            # 静态编译的 Linux 可执行文件
#   config.json             # 默认配置（取自 config.example.json）
#   cookie-proxy.service    # systemd unit
#   install.sh              # 安装 / 升级 / 卸载脚本
#   VERSION                 # 版本号
#
# 部署示例（在目标 Linux 机器上）：
#   tar -xzf cookie-proxy-*.tar.gz && cd cookie-proxy-*/
#   sudo ./install.sh
#
# 用法：
#   ./build.sh [-a 架构] [-v 版本] [-o 输出目录]
#
set -euo pipefail

APP_NAME="cookie-proxy"
GOOS="linux"
GOARCH="amd64"
VERSION=""
OUTPUT_DIR="dist"
CGO_ENABLED="0"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

die() {
	printf '错误: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
用法: ./build.sh [选项]

选项:
  -a, --arch ARCH      目标架构：amd64/x86_64、arm64/aarch64、arm、386、
                       riscv64、mips、ppc64le、s390x 等（默认 amd64）
  -v, --version VER    显式指定版本号（默认取自 git describe）
  -o, --output DIR     输出目录（默认 dist）
  -h, --help           显示帮助
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		-a|--arch)
			GOARCH="${2:?缺少架构参数}"
			shift 2
			;;
		-v|--version)
			VERSION="${2:?缺少版本参数}"
			shift 2
			;;
		-o|--output)
			OUTPUT_DIR="${2:?缺少输出目录参数}"
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			die "未知参数: $1（-h 查看用法）"
			;;
	esac
done

# 架构别名归一化
case "${GOARCH}" in
	amd64|x86_64)
		GOARCH="amd64"
		;;
	arm64|aarch64)
		GOARCH="arm64"
		;;
	arm|386|ppc64le|s390x|riscv64|mips|mipsle|mips64|mips64le)
		;;
	*)
		die "不支持的目标架构: ${GOARCH}"
		;;
esac

# 版本号：优先显式指定，其次取 git 描述
if [[ -z "${VERSION}" ]]; then
	if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
		VERSION="$(git describe --tags --always --dirty 2>/dev/null || printf 'dev')"
	else
		VERSION="dev"
	fi
fi
VERSION="${VERSION//\//-}"

command -v go >/dev/null 2>&1 || die "未找到 go，请先安装 Go 工具链"
[[ -f go.mod ]] || die "请在项目根目录运行本脚本"
[[ -f config.example.json ]] || die "缺少 config.example.json"

STAGE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cookie-proxy-build.XXXXXX")"
trap 'rm -rf "${STAGE_DIR}"' EXIT

PKG_ROOT="${STAGE_DIR}/${APP_NAME}-${VERSION}-${GOOS}-${GOARCH}"
mkdir -p "${PKG_ROOT}"

echo "==> 编译 ${APP_NAME} (${GOOS}/${GOARCH}, CGO_ENABLED=${CGO_ENABLED})"
GOOS="${GOOS}" GOARCH="${GOARCH}" CGO_ENABLED="${CGO_ENABLED}" \
	go build -trimpath -ldflags="-s -w" -o "${PKG_ROOT}/${APP_NAME}" .

echo "==> 复制默认配置"
cp config.example.json "${PKG_ROOT}/config.json"
printf '%s\n' "${VERSION}" >"${PKG_ROOT}/VERSION"

echo "==> 生成 systemd unit"
cat >"${PKG_ROOT}/${APP_NAME}.service" <<'UNIT'
[Unit]
Description=cookie-proxy: Cookie-based HTTP reverse proxy
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=cookie-proxy
Group=cookie-proxy
ExecStart=/usr/local/bin/cookie-proxy -c /etc/cookie-proxy/config.json
Restart=on-failure
RestartSec=3s
# 应用默认有 10 秒优雅退出宽限期，这里留出余量
TimeoutStopSec=20s

# 安全加固
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
UNIT

echo "==> 生成 install.sh"
cat >"${PKG_ROOT}/install.sh" <<'INSTALL_SCRIPT'
#!/usr/bin/env bash
#
# 安装 / 升级 / 卸载 cookie-proxy systemd 服务。
# 用法:
#   sudo ./install.sh               安装或升级（保留已有配置）
#   sudo ./install.sh --uninstall   停止服务并删除安装的文件
# 可选环境变量:
#   BIN_DIR     二进制目录（默认 /usr/local/bin）
#   CONF_DIR    配置目录（默认 /etc/cookie-proxy）
#   UNIT_DIR    unit 目录（默认 /etc/systemd/system）
#   KEEP_USER=1 卸载时保留专用系统用户
#
set -euo pipefail

APP_NAME="cookie-proxy"
SERVICE_NAME="cookie-proxy"
SERVICE_USER="cookie-proxy"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
CONF_DIR="${CONF_DIR:-/etc/cookie-proxy}"
UNIT_DIR="${UNIT_DIR:-/etc/systemd/system}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

need_root() {
	if [[ "$(id -u)" -ne 0 ]]; then
		printf '错误: 请以 root 运行（例如 sudo ./install.sh）\n' >&2
		exit 1
	fi
}

uninstall() {
	need_root
	[[ "${CONF_DIR}" == "/" || -z "${CONF_DIR}" ]] && {
		printf '错误: 拒绝删除目录 %s\n' "${CONF_DIR}" >&2
		exit 1
	}

	printf '==> 停止并禁用服务 %s\n' "${SERVICE_NAME}"
	if [[ -f "${UNIT_DIR}/${SERVICE_NAME}.service" ]]; then
		systemctl disable --now "${SERVICE_NAME}" >/dev/null 2>&1 || true
	fi
	rm -f "${UNIT_DIR}/${SERVICE_NAME}.service"
	rm -f "${BIN_DIR}/${APP_NAME}"
	rm -rf "${CONF_DIR}"
	if [[ "${KEEP_USER:-0}" != "1" ]] && id "${SERVICE_USER}" >/dev/null 2>&1; then
		userdel "${SERVICE_USER}" 2>/dev/null || true
	fi
	systemctl daemon-reload
	printf '卸载完成。配置目录 %s 已删除。\n' "${CONF_DIR}"
	exit 0
}

[[ "${1:-}" == "--uninstall" ]] && uninstall

need_root

printf '==> 创建系统用户 %s（如不存在）\n' "${SERVICE_USER}"
if ! id "${SERVICE_USER}" >/dev/null 2>&1; then
	useradd --system --no-create-home --home-dir "${CONF_DIR}" \
		--shell /usr/sbin/nologin "${SERVICE_USER}"
fi

printf '==> 安装二进制到 %s/%s\n' "${BIN_DIR}" "${APP_NAME}"
install -m 0755 "${SCRIPT_DIR}/${APP_NAME}" "${BIN_DIR}/${APP_NAME}"

printf '==> 安装配置到 %s\n' "${CONF_DIR}"
mkdir -p "${CONF_DIR}"
if [[ -f "${CONF_DIR}/config.json" ]]; then
	printf '    检测到已有配置：保留原文件，新配置另存为 config.json.new\n'
	install -m 0640 -o root -g "${SERVICE_USER}" \
		"${SCRIPT_DIR}/config.json" "${CONF_DIR}/config.json.new"
else
	install -m 0640 -o root -g "${SERVICE_USER}" \
		"${SCRIPT_DIR}/config.json" "${CONF_DIR}/config.json"
fi

printf '==> 安装 systemd unit\n'
install -m 0644 "${SCRIPT_DIR}/${APP_NAME}.service" "${UNIT_DIR}/${SERVICE_NAME}.service"

printf '==> 重新加载 systemd 并启动服务\n'
systemctl daemon-reload
systemctl enable --now "${SERVICE_NAME}"

printf '\n安装完成：\n'
printf '  查看状态: systemctl status %s\n' "${SERVICE_NAME}"
printf '  查看日志: journalctl -u %s -f\n' "${SERVICE_NAME}"
printf '  配置文件: %s/config.json（修改后需重启服务）\n' "${CONF_DIR}"
INSTALL_SCRIPT
chmod 0755 "${PKG_ROOT}/install.sh"

echo "==> 打包"
mkdir -p "${OUTPUT_DIR}"
TARBALL="${OUTPUT_DIR}/${APP_NAME}-${VERSION}-${GOOS}-${GOARCH}.tar.gz"
tar -C "${STAGE_DIR}" -czf "${TARBALL}" "$(basename "${PKG_ROOT}")"

if command -v sha256sum >/dev/null 2>&1; then
	SHA_CMD="sha256sum"
else
	SHA_CMD="shasum -a 256"
fi
(
	cd "${OUTPUT_DIR}"
	${SHA_CMD} "$(basename "${TARBALL}")" >"$(basename "${TARBALL}").sha256"
)

echo
echo "打包完成:"
echo "  ${TARBALL}"
echo "  ${TARBALL}.sha256"
echo
echo "部署到目标机器:"
echo "  tar -xzf ${TARBALL}"
echo "  cd ${APP_NAME}-${VERSION}-${GOOS}-${GOARCH}"
echo "  sudo ./install.sh"
