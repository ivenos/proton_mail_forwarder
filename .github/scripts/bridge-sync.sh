#!/bin/sh
# Follows a Proton Mail Bridge release: its app version, its versions and replacements of the Proton modules,
# and its newer versions of other modules both builds use. Without a tag it takes the latest release, never an older one.
set -eu

main=src/main.go

die() {
    echo "bridge-sync: $*" >&2
    exit 1
}

fetch() {
    if [ -n "${GH_TOKEN:-}" ]; then
        curl -fsSL -H "Authorization: Bearer $GH_TOKEN" "$1"
    else
        curl -fsSL "$1"
    fi
}

current=$(sed -n 's/^const bridgeVersion = "\(.*\)"$/\1/p' "$main")
[ -n "$current" ] || die "bridgeVersion not found in $main"

if [ $# -gt 0 ]; then
    tag=$1
else
    latest=$(fetch https://api.github.com/repos/ProtonMail/proton-bridge/releases/latest) || die "cannot read the latest Bridge release"
    tag=$(printf '%s' "$latest" | tr -d '\n' | sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p')
fi
echo "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "not a release tag such as v3.27.1: $tag"
new="${tag#v}"

if [ $# -eq 0 ] && [ "$(printf '%s\n%s\n' "$current" "$new" | sort -V | tail -n 1)" != "$new" ]; then
    echo "Bridge $new is older than $current, nothing to do."
    exit 0
fi

gomod=$(fetch "https://raw.githubusercontent.com/ProtonMail/proton-bridge/$tag/go.mod") || die "cannot read go.mod of $tag"

replaces=$(printf '%s\n' "$gomod" | awk '
    function emit(line,    f, at) {
        split(line, f, " ")
        at = f[2] == "=>" ? 2 : f[3] == "=>" ? 3 : 0
        if (!at) return
        if (f[at + 2] == "" || f[at + 1] ~ /^[.\/]/) {
            print "bridge-sync: skipping the local replacement of " f[1] > "/dev/stderr"
            return
        }
        print f[1], f[at + 1], f[at + 2]
    }
    /^replace \($/ { on = 1; next }
    on && /^\)$/ { on = 0; next }
    on { emit($0); next }
    /^replace / { sub(/^replace /, ""); emit($0) }')
replaced=$(printf '%s\n' "$replaces" | cut -d ' ' -f 1)
requires=$(printf '%s\n' "$gomod" | awk '/^require \($/ { on = 1; next } on && /^\)$/ { on = 0 } on && $2 ~ /^v/ { print $1, $2 }')

follow() {
    modules=$(go list -m -f '{{.Path}} {{.Version}}' all)
    proton_deps=$(go mod graph | awk '$1 ~ /^github\.com\/ProtonMail\// { sub(/@.*/, "", $2); print $2 }' | sort -u)

    for mod in $(go list -m -f '{{if .Replace}}{{.Path}}{{end}}' all); do
        printf '%s\n' "$replaced" | grep -qxF "$mod" || go mod edit -dropreplace="$mod"
    done

    printf '%s\n' "$replaces" | while read -r from to ver; do
        [ -n "$from" ] || continue
        printf '%s\n' "$proton_deps" | grep -qxF "$from" || continue
        case "$to" in
            github.com/ProtonMail/*) go mod edit -replace="$from=$to@$ver" ;;
            *) die "Bridge replaces $from with $to, which is not a Proton module. Check it before following." ;;
        esac
    done

    printf '%s\n' "$requires" | while read -r mod ver; do
        ours=$(printf '%s\n' "$modules" | awk -v m="$mod" '$1 == m { print $2 }')
        [ -n "$ours" ] || continue
        case "$mod" in
            github.com/ProtonMail/*) ;;
            *)
                if printf '%s\n' "$replaced" | grep -qxF "$mod"; then
                    printf '%s\n' "$proton_deps" | grep -qxF "$mod" || continue
                elif [ "$(printf '%s\n%s\n' "$ours" "$ver" | sort -V | tail -n 1)" != "$ver" ]; then
                    continue
                fi
                ;;
        esac
        [ "$ours" = "$ver" ] || go mod edit -require="$mod@$ver"
    done

    go mod tidy
}

pass=0
while :; do
    before=$(cat go.mod)
    follow
    [ "$(cat go.mod)" != "$before" ] || break
    pass=$((pass + 1))
    [ "$pass" -lt 5 ] || die "go.mod keeps changing"
done

awk -v v="$new" '/^const bridgeVersion = / { $0 = "const bridgeVersion = \"" v "\"" } { print }' "$main" > "$main.tmp"
mv "$main.tmp" "$main"
if [ "$new" != "$current" ]; then
    sh .github/scripts/changelog.sh dependency proton-bridge "$current" "$new"
fi

echo "Following Proton Mail Bridge $new"
