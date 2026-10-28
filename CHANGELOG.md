# Changelog

All notable changes to this project are documented in this file, in the format of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- An SMTP server that hands every message to Proton Mail for the inbox, with a notification, and removes the copy in Sent.
- `login` signs in once, with two-factor code and CAPTCHA if Proton asks for them.
- `logout` ends the session and deletes it.
- An optional username and password for the apps that send.
- A healthcheck that fails while mail cannot be delivered, a copy stays under Sent or the forwarder hangs.
- Each new Proton Mail Bridge release is followed on its own.
- An image for amd64 and arm64 on Docker Hub.

### Dependencies

- golang.org/x/net v0.56.0 -> v0.61.0

[Unreleased]: https://github.com/ivenos/proton_mail_forwarder/commits/main
