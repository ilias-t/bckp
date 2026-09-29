#!/bin/sh
# Install backpack (bckp): keep a lid-closed Mac awake without cooking it.
# https://github.com/ilias-t/bckp
#
#   curl -fsSL https://raw.githubusercontent.com/ilias-t/bckp/main/install.sh | sh
#   sh install.sh --uninstall
#
# Environment:
#   BACKPACK_VERSION    release to install, e.g. v0.1.0 (default: the latest)
#   BACKPACK_BIN_DIR    where the binaries go (default: ~/.local/bin)
#   BACKPACK_TARBALL    install this local tarball instead of downloading one;
#                       its checksums.txt must sit next to it
#   BACKPACK_SKIP_SUDO  1 skips `sudo backpack install|uninstall` (for testing)
#
# The tarball is checked against the release's checksums.txt. Releases also
# carry a GitHub build provenance attestation; to check that too, download
# the tarball and run (needs the gh CLI):
#   gh attestation verify bckp_<version>_darwin_universal.tar.gz --repo ilias-t/bckp
#
# Everything runs from main, called on the last line, so a download cut
# short runs nothing.
set -eu

REPO=ilias-t/bckp

say() { printf '%s\n' "$*"; }
die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

# fetch [curl args...] URL: HTTPS only, TLS 1.2 or later.
fetch() {
	curl --proto '=https' --tlsv1.2 -fsSL "$@"
}

# valid_tag TAG succeeds for a release tag such as v1.2.3 or v1.2.3-rc.1.
valid_tag() {
	case $1 in *[!0-9A-Za-z.-]*) return 1 ;; esac
	printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}

uninstall() {
	if [ "${BACKPACK_SKIP_SUDO:-}" != 1 ] && [ -x "$BIN_DIR/backpack" ]; then
		say "Removing the root helper; sudo asks for your password."
		sudo "$BIN_DIR/backpack" uninstall
	fi
	rm -f "$BIN_DIR/backpack" "$BIN_DIR/backpackd" "$BIN_DIR/bckp"
	say "Removed backpack from $BIN_DIR."
	say "Your config (~/.config/backpack) and logs (~/Library/Logs/backpack) are left; delete them if you like."
}

main() {
	BIN_DIR=${BACKPACK_BIN_DIR:-$HOME/.local/bin}
	[ "$(uname -s)" = Darwin ] || die "backpack runs on macOS only"

	if [ "${1:-}" = --uninstall ]; then
		uninstall
		return
	fi
	[ $# -eq 0 ] || die "usage: install.sh [--uninstall]"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	# Get the tarball and its checksums.
	if [ -n "${BACKPACK_TARBALL:-}" ]; then
		tarball=$BACKPACK_TARBALL
		sums=$(dirname "$tarball")/checksums.txt
	else
		tag=${BACKPACK_VERSION:-}
		if [ -z "$tag" ]; then
			tag=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
				sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
			[ -n "$tag" ] || die "could not find the latest release of $REPO"
		fi
		case $tag in v*) ;; *) tag=v$tag ;; esac
		valid_tag "$tag" || die "not a release version: $tag (want e.g. v1.2.3)"
		tarball=$tmp/bckp_${tag#v}_darwin_universal.tar.gz
		sums=$tmp/checksums.txt
		url=https://github.com/$REPO/releases/download/$tag
		say "Downloading backpack $tag..."
		fetch -o "$tarball" "$url/$(basename "$tarball")" || die "download failed: $url/$(basename "$tarball")"
		fetch -o "$sums" "$url/checksums.txt" || die "download failed: $url/checksums.txt"
	fi

	# Verify it.
	name=$(basename "$tarball")
	[ -f "$sums" ] || die "missing $sums"
	want=$(awk -v f="$name" '{ n = $2; sub(/^\*/, "", n) } n == f { print $1 }' "$sums")
	[ -n "$want" ] || die "no checksum for $name in $sums"
	got=$(shasum -a 256 "$tarball" | awk '{ print $1 }')
	[ "$got" = "$want" ] || die "checksum mismatch for $name"
	say "Checksum OK ($name)."
	say "Optional provenance check (gh CLI): gh attestation verify $name --repo $REPO"

	mkdir -p "$tmp/x"
	tar -xzf "$tarball" -C "$tmp/x" backpack backpackd
	if [ "${BACKPACK_SKIP_SUDO:-}" != 1 ] && "$tmp/x/backpack" status >/dev/null 2>&1; then
		die "a backpack session is running; stop it first (bckp stop), then run the installer again"
	fi

	# The root helper is the only part that needs root. It is installed from
	# the verified copy in the temp dir, before anything lands in BIN_DIR.
	say ""
	if [ "${BACKPACK_SKIP_SUDO:-}" = 1 ]; then
		say "Skipping the root helper (BACKPACK_SKIP_SUDO=1). Later: sudo $BIN_DIR/backpack install"
	else
		say "backpack installs a small root helper that can only toggle lid-close sleep; sudo asks for your password once."
		sudo "$tmp/x/backpack" install --no-hint --daemon-bin "$tmp/x/backpackd" ||
			die "sudo backpack install failed; nothing was installed in $BIN_DIR"
	fi

	# Install the binaries. A new file is moved into place, so a running
	# backpack keeps its old binary intact.
	say ""
	mkdir -p "$BIN_DIR"
	for b in backpack backpackd; do
		cp "$tmp/x/$b" "$BIN_DIR/.$b.new"
		chmod 0755 "$BIN_DIR/.$b.new"
		mv -f "$BIN_DIR/.$b.new" "$BIN_DIR/$b"
	done
	ln -sfn backpack "$BIN_DIR/bckp"
	say "Installed backpack, backpackd and bckp in $BIN_DIR ($("$BIN_DIR/backpack" version))."

	case ":$PATH:" in
	*":$BIN_DIR:"*) on_path=1 ;;
	*)
		on_path=0
		say ""
		say "$BIN_DIR is not on your PATH. Add it to ~/.zshenv, which also covers \`ssh mac bckp\`:"
		say "  echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshenv"
		;;
	esac

	say ""
	"$BIN_DIR/backpack" doctor || true

	if command -v brew >/dev/null 2>&1 && ! command -v macmon >/dev/null 2>&1; then
		say ""
		say "Optional: brew install macmon for CPU/GPU °C readings."
	fi

	say ""
	if [ "$on_path" = 1 ]; then
		say "Done. Next: bckp"
	else
		say "Done. Next: $BIN_DIR/bckp"
	fi
}

main "$@"
