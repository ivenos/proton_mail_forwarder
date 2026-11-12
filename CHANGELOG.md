# Changelog

All notable changes to this project are documented in this file, in the format of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Dependencies

- golang.org/x/term v0.47.0 -> v0.48.0

## [1.1.0] - 2026-11-12

### Added

- Apps can send from addresses with umlauts or other characters outside ASCII.

### Fixed

- Mail no longer goes to an address that cannot receive mail, such as an external one.
- Replies reach the sender even when the sender's name uses an unknown character set.
- A message without headers, such as the output of a script, arrives with its text.
- An app that stops sending in the middle of a message gets a temporary error instead of a permanent one.
- A large message over a slow connection no longer times out while it is still arriving.
- A missing data folder no longer ends the session at Proton.
- The healthcheck no longer passes while a renewed session could not be saved.
- After a restart, a message Proton Mail did not confirm no longer keeps the healthcheck failing for a day.
- Pressing Ctrl-C at the password prompt no longer leaves the terminal without echo.
- `logout` no longer blames Proton when the data folder cannot be used.

### Dependencies

- github.com/emersion/go-smtp v0.25.0 -> v0.25.1 (#3)
- golang.org/x/sys v0.49.0 -> v0.50.0 (#4)
- alpine 3.24.3 -> 3.24.4 (#5)
- go 1.27.3 -> 1.27.4 (#5)

## [1.0.0] - 2026-10-31

### Added

- An SMTP server that hands every message to Proton Mail for the inbox, with a notification, and removes the copy in Sent.
- `login` signs in once, with two-factor code and CAPTCHA if Proton asks for them.
- `logout` ends the session and deletes it.
- An optional username and password for the apps that send.
- A healthcheck that fails while mail cannot be delivered, a copy stays under Sent or the forwarder hangs.
- Each new Proton Mail Bridge release is followed on its own.
- An image for amd64 and arm64 on Docker Hub.

[Unreleased]: https://github.com/ivenos/proton_mail_forwarder/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/ivenos/proton_mail_forwarder/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/ivenos/proton_mail_forwarder/releases/tag/v1.0.0
