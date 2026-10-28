# Contributing

## Build

Go as pinned in the `toolchain` line of `go.mod`:

```bash
go build -o proton_mail_forwarder ./src
```

The image, from binaries for amd64 and arm64:

```bash
sh .github/scripts/build-binaries.sh dev dist
docker build -t proton_mail_forwarder:test .
```

## Tests

- The tests run against the fake Proton API from go-proton-api and a fake Proton Mail SMTP server. They need no Proton account:

```bash
go test -race ./...
```

- The image test starts the container without a session and checks that it listens, refuses mail for now and reports itself unhealthy:

```bash
sh test/image_test.sh proton_mail_forwarder:test
```

- Lint and known vulnerabilities:

```bash
gofmt -l .
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
shellcheck test/image_test.sh .github/scripts/bridge-sync.sh .github/scripts/build-binaries.sh .github/scripts/dockerhub-description.sh
```

- CI runs all of it, the tests also on Windows and the image test on amd64 and arm64, before anything is published.

## Code style

- `gofmt`, standard library first.
- A comment only earns its place when it records something the code cannot. One or two lines.
- Nothing logs message content or addresses.

## Proton Mail Bridge

- The app version sent to Proton is `linux-bridge@` plus `bridgeVersion` in `src/main.go`.
- The Proton modules, and the replacements Bridge puts on modules they use, carry the versions of that Bridge release. Other modules both use go up to Bridge's version, never down.
- `.github/scripts/bridge-sync.sh [tag]` sets all of it from a Bridge release and adds the changelog line. Never change them by hand.
- It stops when Bridge replaces a module the forwarder uses with one outside `github.com/ProtonMail`.
- The Follow Proton Mail Bridge workflow runs it every day, commits to the `bridge-sync` branch, runs CI there, moves `main` to it and releases a patch.

## Environment variables

- A new variable touches `loadConfig` in `src/main.go`, a case in `TestLoadConfig` and a row in the README table.
- A variable that is not in the README does not exist.

## Commits

- Conventional Commits (https://www.conventionalcommits.org/en/v1.0.0/) with a short imperative subject.
- A commit that does more than one thing lists its changes in the body, under the changelog section names as plain words. Changes to CI and tests are listed there too:

```
Added
- logout revokes the session at Proton

Changed
- CI runs the image test on arm64

Fixed
- A refreshed session was not saved
```

## Changelog

- `CHANGELOG.md` follows Keep a Changelog (https://keepachangelog.com/en/1.1.0/), with `Breaking changes` as the first section and `Dependencies` as the last.
- Every commit that changes something for users adds its entry under `Unreleased`. CI, tests and refactoring stay out.
- An entry is one short sentence in plain words that anyone understands. Details belong in the README.
- What users have to act on after an update goes under `Breaking changes`.
- An entry that goes back to an issue or a pull request ends with its number: `(#12)`.

## Releases

- Run the Release workflow with the new version, such as `1.5.0`. It turns `Unreleased` into that version, tags the commit, publishes the image and builds a draft release with the notes from `CHANGELOG.md`.
- A tag pushed by hand publishes the image and builds the draft too, from the section of its version.
- An update of Alpine, Go or Proton Mail Bridge is released on its own as the next patch version, without a draft, if `Unreleased` lists nothing but dependencies. Otherwise it waits for the next release.
- The image goes to Docker Hub and GHCR, for amd64 and arm64. A release moves `latest`, a prerelease such as `v2.0.0-rc1` does not. Commits on `main` publish `dev`.
- A release also sets the Docker Hub description: logo and introduction from `README.md` and a link to the repository.

## Dependencies

- The Alpine base, Go and GitHub Actions stay on version tags, never commit SHAs or digests.
- Renovate opens the bumps and adds those that reach users to the changelog, not GitHub Actions or test tools. The release adds the numbers of their pull requests. Other PRs leave dependencies alone.
- Renovate leaves the Proton modules alone, they follow Proton Mail Bridge.

## Pull requests

- One concern per PR, with tests and a changelog entry for behavior changes.
- Only the PR author and the maintainer commit to it.
- `gofmt`, `go vet`, `go test -race ./...` and `shellcheck` must pass.
- Fill in the PR template, including the test plan and the CLA checkbox.
