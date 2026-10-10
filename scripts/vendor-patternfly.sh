#!/bin/sh
# Vendors the PatternFly CSS and the fonts it references into web/static/patternfly/.
# Needs only curl, tar and sha256sum; no Node. Run from the repository root:
#
#   scripts/vendor-patternfly.sh
#
# To upgrade, change PF_VERSION and PF_SHA256 (the sha256 of the npm tarball), rerun the
# script (it also updates the version in NOTICE) and commit the result. A major version
# renames every pf-vN class, so the templates in web/templates/ must be updated too.
set -eu

PF_VERSION=6.6.1
PF_SHA256=6ee4b5baac7e1319f121b2e95aaf76a24c7a4525e80185468f59300aabf4eb14

dest=web/static/patternfly
if [ ! -d web/static ]; then
	echo "run this from the repository root" >&2
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/pf.tgz" \
	"https://registry.npmjs.org/@patternfly/patternfly/-/patternfly-$PF_VERSION.tgz"
echo "$PF_SHA256  $tmp/pf.tgz" | sha256sum -c -
tar -xzf "$tmp/pf.tgz" -C "$tmp"
pkg=$tmp/package

# Only the stylesheet and the fonts it references. The background images of the glass and
# felt themes and the PatternFly logos are left out (the logos are trademarks). The license
# texts in $dest are kept; they don't come from the npm package.
rm -rf "$dest/assets" "$dest/patternfly.min.css"
mkdir -p "$dest/assets/fonts/RedHatDisplay" "$dest/assets/fonts/RedHatText" \
	"$dest/assets/fonts/RedHatMono" "$dest/assets/fonts/webfonts" "$dest/assets/pficon"
# The source map isn't vendored, so drop the comment pointing at it.
sed '/sourceMappingURL=/d' "$pkg/patternfly.min.css" > "$dest/patternfly.min.css"
for f in RedHatDisplay/RedHatDisplayVF.woff2 RedHatDisplay/RedHatDisplayVF-Italic.woff2 \
	RedHatText/RedHatTextVF.woff2 RedHatText/RedHatTextVF-Italic.woff2 \
	RedHatMono/RedHatMonoVF.woff2 RedHatMono/RedHatMonoVF-Italic.woff2 \
	webfonts/fa-solid-900.woff2; do
	cp "$pkg/assets/fonts/$f" "$dest/assets/fonts/$f"
done
cp "$pkg/assets/pficon/pf-v6-pficon.woff2" "$dest/assets/pficon/"
echo "$PF_VERSION" > "$dest/VERSION"
# Keep the version named in NOTICE in step.
sed "s|@patternfly/patternfly [0-9][0-9.]*|@patternfly/patternfly $PF_VERSION|" NOTICE > "$tmp/NOTICE"
cat "$tmp/NOTICE" > NOTICE

# Every font the stylesheet points at must have been copied.
grep -o 'url(assets/[^)]*)' "$dest/patternfly.min.css" | sed 's/^url(//; s/)$//' | sort -u |
	while read -r f; do
		if [ ! -f "$dest/$f" ]; then
			echo "missing $f: update the copy list above" >&2
			exit 1
		fi
	done
echo "vendored PatternFly $PF_VERSION into $dest"
