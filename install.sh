#!/bin/sh
# Installs or updates Switcher on this Mac, also over SSH:
#
#   curl -fsSL https://raw.githubusercontent.com/00xmario/switcher/main/install.sh | sh
#
# It downloads the latest release, checks its checksum, copies Switcher.app to
# /Applications (or ~/Applications without write access), puts the switcher
# command on your PATH and starts Switcher. A running Switcher is quit first,
# which interrupts requests that are still in flight.
set -eu

repo=00xmario/switcher

fail() {
	echo "$*" >&2
	exit 1
}

# stop_switcher quits the app in $1 and waits until it has exited.
stop_switcher() {
	pattern="$1/Contents/MacOS/"
	pgrep -f "$pattern" >/dev/null 2>&1 || return 0
	osascript -e 'quit app "Switcher"' >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -f "$pattern" >/dev/null 2>&1 || return 0
		sleep 1
	done
	# Over SSH macOS often refuses the quit request; stop the processes.
	pkill -f "$pattern" >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -f "$pattern" >/dev/null 2>&1 || return 0
		sleep 1
	done
	fail "Switcher did not quit; quit it from its menu and run this again."
}

main() {
	[ "$(uname -s)" = Darwin ] || fail "The Switcher app is for macOS. On Linux, download switcher-server from https://github.com/$repo/releases"
	[ "$(uname -m)" = arm64 ] || fail "The Switcher app needs a Mac with Apple silicon."

	tag=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$tag" ] || fail "Could not find the latest Switcher release."
	version=${tag#v}
	dmg="Switcher_$version.dmg"

	tmp=$(mktemp -d)
	trap 'hdiutil detach -quiet "$tmp/mnt" 2>/dev/null || true; rm -rf "$tmp"' EXIT

	echo "Downloading Switcher $version..."
	curl -fsSL -o "$tmp/$dmg" "https://github.com/$repo/releases/download/$tag/$dmg"
	if curl -fsSL -o "$tmp/$dmg.sha256" "https://github.com/$repo/releases/download/$tag/$dmg.sha256" 2>/dev/null; then
		(cd "$tmp" && shasum -a 256 -c "$dmg.sha256" >/dev/null) || fail "The download does not match its checksum; nothing was installed."
	else
		echo "This release publishes no checksum for the app; installing it without that check."
	fi

	apps=/Applications
	if [ ! -w "$apps" ]; then
		apps="$HOME/Applications"
		mkdir -p "$apps"
	fi
	hdiutil attach -quiet -nobrowse -readonly -mountpoint "$tmp/mnt" "$tmp/$dmg"
	[ -d "$tmp/mnt/Switcher.app" ] || fail "The download contains no Switcher.app; nothing was installed."
	if [ -d "$apps/Switcher.app" ]; then
		stop_switcher "$apps/Switcher.app"
		rm -rf "$apps/Switcher.app"
	fi
	ditto "$tmp/mnt/Switcher.app" "$apps/Switcher.app"

	server="$apps/Switcher.app/Contents/MacOS/SwitcherServer"
	# Releases before the switcher command treat any argument as "serve".
	if ! grep -q "install-cli" "$server"; then
		open -g "$apps/Switcher.app" 2>/dev/null || true
		echo "Switcher $version is installed. The switcher command arrives with the next release."
		return 0
	fi
	"$server" install-cli
	"$server" start
	if "$server" status --json 2>/dev/null | grep -q "\"version\":\"$version\""; then
		echo
		echo "Switcher $version is installed and running."
	else
		echo
		echo "Switcher $version is installed, but another Switcher still answers on this Mac; quit it and run: switcher start"
	fi
	echo "Next: switcher login claude, or switcher connect <host> --code <code>"
}

main "$@"
