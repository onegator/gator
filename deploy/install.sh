#!/bin/sh
# gator installer for Linux hosts with systemd.
#
#   install.sh install server|runner [--version vX.Y.Z] [--archive PATH] [--checksums PATH]
#   install.sh migrate            run database migrations as the gator user
#   install.sh exec ARGS...       run `gator-server ARGS...` as the gator user with server.env
#   install.sh uninstall server|runner
#
# Idempotent: re-running `install` upgrades the binary, keeps existing env files and
# restarts the unit only if it was running. Downloads are verified against checksums.txt.
set -eu

REPO="${GATOR_REPO:-onegator/gator}"
PREFIX="${GATOR_PREFIX:-/usr/local}"
ETC="${GATOR_ETC:-/etc/gator}"
UNIT_DIR="${GATOR_UNIT_DIR:-/etc/systemd/system}"
LIB="$PREFIX/lib/gator"

# Logs go to stderr so functions whose stdout is captured (fetch) return clean values.
log() { printf 'gator-install: %s\n' "$*" >&2; }
die() { printf 'gator-install: error: %s\n' "$*" >&2; exit 1; }

need_root() { [ "$(id -u)" -eq 0 ] || die "run as root (sudo)"; }

has_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
	esac
}

latest_version() {
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "cannot resolve latest release"
	echo "${url##*/}"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# fetch COMPONENT VERSION ARCHIVE CHECKSUMS WORKDIR → prints path of the verified archive
fetch() {
	component=$1 version=$2 archive=$3 checksums=$4 work=$5
	os=linux arch=$(detect_arch)
	name="gator-${component}_${version#v}_${os}_${arch}.tar.gz"
	if [ -z "$archive" ]; then
		base="https://github.com/$REPO/releases/download/$version"
		log "downloading $name"
		curl -fsSL -o "$work/$name" "$base/$name" || die "download failed: $base/$name"
		curl -fsSL -o "$work/checksums.txt" "$base/checksums.txt" || die "checksums download failed"
		archive="$work/$name"
		checksums="$work/checksums.txt"
	fi
	[ -f "$archive" ] || die "archive not found: $archive"
	if [ -n "$checksums" ]; then
		want=$(grep " $(basename "$archive")\$" "$checksums" | cut -d' ' -f1)
		[ -n "$want" ] || die "no checksum for $(basename "$archive")"
		got=$(sha256_of "$archive")
		[ "$want" = "$got" ] || die "checksum mismatch for $(basename "$archive")"
		log "checksum ok"
	else
		log "warning: no checksums supplied; archive not verified"
	fi
	echo "$archive"
}

ensure_user() {
	user=$1 home=$2 shell=$3
	if ! id "$user" >/dev/null 2>&1; then
		useradd --system --home-dir "$home" --create-home --shell "$shell" --user-group "$user"
		log "created user $user"
	fi
}

install_env() {
	example=$1 target=$2
	mkdir -p "$ETC"
	chmod 0755 "$ETC"
	if [ -f "$target" ]; then
		log "keeping existing $target"
		return 1
	fi
	install -m 0600 -o root -g root "$example" "$target"
	log "wrote $target (mode 0600); edit it before starting"
	return 0
}

cmd_install() {
	component=${1:-}
	shift || true
	case "$component" in server | runner) ;; *) die "usage: install.sh install server|runner [...]" ;; esac
	version="" archive="" checksums=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--version) version=$2; shift 2 ;;
		--archive) archive=$2; shift 2 ;;
		--checksums) checksums=$2; shift 2 ;;
		*) die "unknown flag $1" ;;
		esac
	done
	need_root
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT
	if [ -z "$archive" ] && [ -z "$version" ]; then
		version=$(latest_version)
	fi
	[ -n "$version" ] || version="local"
	path=$(fetch "$component" "$version" "$archive" "$checksums" "$work")
	mkdir -p "$work/x"
	tar -xzf "$path" -C "$work/x"

	bin="gator-$component"
	install -m 0755 "$work/x/$bin" "$PREFIX/bin/$bin"
	log "installed $PREFIX/bin/$bin ($("$PREFIX/bin/$bin" version))"
	mkdir -p "$LIB"
	install -m 0755 "$work/x/deploy/install.sh" "$LIB/install.sh"

	fresh_env=1
	if [ "$component" = server ]; then
		ensure_user gator /var/lib/gator /usr/sbin/nologin
		if install_env "$work/x/deploy/server.env.example" "$ETC/server.env"; then
			key=$("$PREFIX/bin/gator-server" secrets new-key)
			sed -i "s|^GATOR_SECRETS_KEY=.*|GATOR_SECRETS_KEY=$key|" "$ETC/server.env"
			log "generated GATOR_SECRETS_KEY (id ${key%%:*}); back it up, data is unreadable without it"
		else
			fresh_env=0
		fi
	else
		ensure_user gator-runner /var/lib/gator-runner /bin/bash
		mkdir -p /var/lib/gator-runner/work
		chown -R gator-runner:gator-runner /var/lib/gator-runner
		install_env "$work/x/deploy/runner.env.example" "$ETC/runner.env" || fresh_env=0
	fi

	install -m 0644 "$work/x/deploy/systemd/$bin.service" "$UNIT_DIR/$bin.service"
	if has_systemd; then
		systemctl daemon-reload
		systemctl enable "$bin.service" >/dev/null
		if systemctl is-active --quiet "$bin.service"; then
			systemctl restart "$bin.service"
			log "restarted $bin.service"
		fi
	else
		log "systemd not running here; unit installed to $UNIT_DIR, enable it on the host"
	fi

	if [ "$fresh_env" -eq 1 ]; then
		log "next: edit $ETC/$component.env"
		[ "$component" = server ] && log "then: sudo $LIB/install.sh migrate"
		log "then: sudo systemctl start $bin"
	elif [ "$component" = server ]; then
		log "upgrade: run 'sudo $LIB/install.sh migrate' if this release adds migrations"
	fi
}

# cmd_exec runs gator-server as the gator user with the server environment loaded.
cmd_exec() {
	need_root
	[ -f "$ETC/server.env" ] || die "$ETC/server.env missing"
	command -v runuser >/dev/null 2>&1 || die "runuser not found"
	set -a
	# shellcheck disable=SC1091
	. "$ETC/server.env"
	set +a
	runuser -u gator -- "$PREFIX/bin/gator-server" "$@"
}

cmd_migrate() {
	cmd_exec migrate
	log "migrations applied"
}

cmd_uninstall() {
	component=${1:-}
	case "$component" in server | runner) ;; *) die "usage: install.sh uninstall server|runner" ;; esac
	need_root
	bin="gator-$component"
	if has_systemd; then
		systemctl disable --now "$bin.service" >/dev/null 2>&1 || true
	fi
	rm -f "$UNIT_DIR/$bin.service" "$PREFIX/bin/$bin"
	has_systemd && systemctl daemon-reload
	log "removed $bin; kept $ETC/$component.env, users and data"
}

case "${1:-}" in
install) shift; cmd_install "$@" ;;
migrate) cmd_migrate ;;
exec) shift; cmd_exec "$@" ;;
uninstall) shift; cmd_uninstall "$@" ;;
*) die "usage: install.sh install|migrate|exec|uninstall ..." ;;
esac
