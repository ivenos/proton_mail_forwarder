#!/bin/sh
# Builds the Linux binaries the image is made from, and the license texts of everything compiled into them.
set -eu

version="${1:?usage: build-binaries.sh VERSION DIR}"
out="${2:?usage: build-binaries.sh VERSION DIR}"
mkdir -p "$out"

for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" \
        -o "$out/proton_mail_forwarder-linux-$arch" ./src
done

goversion=$(go env GOVERSION)
modules=$(
    printf 'Go %s\n%s\n' "${goversion#go}" "$(go env GOROOT)"
    for arch in amd64 arm64; do
        GOOS=linux GOARCH="$arch" go list -deps \
            -f '{{with .Module}}{{if not .Main}}{{with or .Replace .}}{{.Path}} {{.Version}}{{"\n"}}{{.Dir}}{{end}}{{end}}{{end}}' ./src
    done | awk 'NR % 2 { name = $0; next } !seen[name]++ { print name; print }'
)

printf '%s\n' "$modules" | {
    printf 'Proton Mail Forwarder contains Go and the modules below. Their license texts follow.\n'
    while read -r name && read -r dir; do
        found=
        for file in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do
            [ -f "$file" ] || continue
            printf '\n\n==== %s: %s ====\n\n' "$name" "$(basename "$file")"
            cat "$file"
            found=1
        done
        [ -n "$found" ] || { echo "build-binaries: no license text for $name" >&2; exit 1; }
    done
} > "$out/THIRD_PARTY_NOTICES"
