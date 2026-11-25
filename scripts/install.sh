#!/bin/sh
# Install tern from GitHub releases.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/DeadLaurin/tern/main/scripts/install.sh | sh
#   # Or host this file directly on your own site:
#   curl -fsSL https://yoursite.com/install.sh | sh
#
# Environment:
#   TERN_VERSION      Release version to install (e.g. "1.2.3" or "v1.2.3").
#                     Defaults to the latest release.
#   TERN_INSTALL_DIR  Directory to install the tern binary into.
#                     Defaults to /usr/local/bin when writable, otherwise
#                     $HOME/.local/bin.
#   TERN_REPO         GitHub repository ("owner/name"). Defaults to DeadLaurin/tern.
#
# This script is POSIX sh compatible. It requires curl or wget, tar, and
# either sha256sum or shasum.

set -eu

REPO="${TERN_REPO:-DeadLaurin/tern}"
BASE_URL="https://github.com/${REPO}"
DOWNLOADS_URL="${BASE_URL}/releases/download"

err() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

info() {
	printf '%s\n' "$*"
}

need() {
	command -v "$1" >/dev/null 2>&1
}

# download URL DEST
download() {
	_url=$1
	_dest=$2

	if need curl; then
		curl -fsSL --proto '=https' --tlsv1.2 -o "$_dest" "$_url"
	elif need wget; then
		wget -q -O "$_dest" "$_url"
	else
		err "curl or wget is required to download tern"
	fi
}

# fetch URL - print body to stdout
fetch() {
	_url=$1

	if need curl; then
		curl -fsSL --proto '=https' --tlsv1.2 "$_url"
	elif need wget; then
		wget -q -O - "$_url"
	else
		err "curl or wget is required to download tern"
	fi
}

resolve_latest_version() {
	# Prefer the redirect target of /releases/latest: it avoids the API rate
	# limit and does not require jq. Fall back to the API for wget.
	if need curl; then
		final_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "${BASE_URL}/releases/latest" 2>/dev/null) ||
			err "could not query the latest tern release at ${BASE_URL}/releases"
		tag=${final_url##*/}
	else
		json=$(fetch "https://api.github.com/repos/${REPO}/releases/latest") ||
			err "could not query the latest tern release at ${BASE_URL}/releases"
		tag=$(printf '%s' "$json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	fi

	[ -n "${tag:-}" ] || err "could not determine the latest tern version"
	case "$tag" in
	v[0-9]*) ;;
	*) err "could not determine the latest tern version (got: ${tag})" ;;
	esac
	printf '%s\n' "${tag#v}"
}

detect_os() {
	case "$(uname -s)" in
	Linux) printf 'linux\n' ;;
	Darwin) printf 'darwin\n' ;;
	*) err "unsupported operating system: $(uname -s) (supported: Linux, macOS)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) printf 'amd64\n' ;;
	arm64 | aarch64) printf 'arm64\n' ;;
	*) err "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
	esac
}

default_install_dir() {
	if [ -n "${TERN_INSTALL_DIR:-}" ]; then
		printf '%s\n' "$TERN_INSTALL_DIR"
	elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		printf '/usr/local/bin\n'
	else
		printf '%s\n' "$HOME/.local/bin"
	fi
}

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t tern-install)
cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

version=${TERN_VERSION:-}
version=$(printf '%s' "$version" | tr -d '[:space:]')
if [ -z "$version" ]; then
	info "Looking up the latest tern release..."
	version=$(resolve_latest_version)
fi
version=${version#v}
[ -n "$version" ] || err "could not determine the tern version to install"
tag="v${version}"

os=$(detect_os)
arch=$(detect_arch)

asset="tern_${version}_${os}_${arch}.tar.gz"
archive="${tmp_dir}/${asset}"
checksums="${tmp_dir}/checksums.txt"
extract_dir="${tmp_dir}/extract"

info "Installing tern ${version} (${os}/${arch})"

download "${DOWNLOADS_URL}/${tag}/${asset}" "$archive" ||
	err "failed to download ${asset} for ${tag}"
download "${DOWNLOADS_URL}/${tag}/checksums.txt" "$checksums" ||
	err "failed to download checksums.txt for ${tag}"

expected=$(awk -v f="$asset" '
	{
		name = $2
		sub(/^\*/, "", name)
		sub(/^\.\//, "", name)
		if (name == f) {
			print tolower($1)
			exit
		}
	}
' "$checksums")

[ -n "$expected" ] || err "no checksum found for ${asset} in checksums.txt"

if need sha256sum; then
	actual=$(sha256sum "$archive" | awk '{ print tolower($1) }')
elif need shasum; then
	actual=$(shasum -a 256 "$archive" | awk '{ print tolower($1) }')
else
	err "sha256sum or shasum is required to verify the download"
fi

if [ "$actual" != "$expected" ]; then
	err "checksum verification failed for ${asset}
  expected: ${expected}
  actual:   ${actual}"
fi

mkdir -p "$extract_dir"
tar -xzf "$archive" -C "$extract_dir" || err "failed to extract ${asset}"

binary="${extract_dir}/tern"
if [ ! -f "$binary" ]; then
	binary=$(find "$extract_dir" -type f -name tern 2>/dev/null | head -n 1 || true)
fi
[ -n "${binary:-}" ] && [ -f "$binary" ] || err "tern binary not found in ${asset}"

install_dir=$(default_install_dir)
mkdir -p "$install_dir" || err "could not create install directory: ${install_dir}"

if need install; then
	install -m 0755 "$binary" "${install_dir}/tern"
else
	cp "$binary" "${install_dir}/tern"
	chmod 0755 "${install_dir}/tern"
fi

info "Installed tern to ${install_dir}/tern"

case ":${PATH:-}:" in
*":${install_dir}:"*)
	;;
*)
	info ""
	info "${install_dir} is not on your PATH. Add it to your shell profile:"
	case "${SHELL:-}" in
	*fish)
		info "  fish: set -Ux PATH ${install_dir} \$PATH"
		;;
	*)
		info "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.profile"
		info "  # or, for interactive shells:"
		info "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.bashrc"
		info "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.zshrc"
		;;
	esac
	info ""
	info "Then restart your shell or run:"
	info "  export PATH=\"${install_dir}:\$PATH\""
	;;
esac
