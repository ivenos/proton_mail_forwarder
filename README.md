<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/assets/proton_mail_forwarder-logo-on-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset=".github/assets/proton_mail_forwarder-logo-on-light.svg">
  <img alt="Proton Mail Forwarder" src=".github/assets/proton_mail_forwarder-logo-auto.svg" width="50%">
</picture>

<a href="https://hub.docker.com/r/ivenos/proton_mail_forwarder"><picture><source media="(prefers-color-scheme: dark)" srcset="https://shieldcn.dev/docker/size/ivenos/proton_mail_forwarder.svg?variant=secondary&amp;mode=dark"><img alt="Docker Image Size" src="https://shieldcn.dev/docker/size/ivenos/proton_mail_forwarder.svg?variant=secondary&amp;mode=light"></picture></a>
<a href="https://hub.docker.com/r/ivenos/proton_mail_forwarder"><picture><source media="(prefers-color-scheme: dark)" srcset="https://shieldcn.dev/docker/pulls/ivenos/proton_mail_forwarder.svg?variant=secondary&amp;mode=dark"><img alt="Docker Pulls" src="https://shieldcn.dev/docker/pulls/ivenos/proton_mail_forwarder.svg?variant=secondary&amp;mode=light"></picture></a>
<a href="LICENSE"><picture><source media="(prefers-color-scheme: dark)" srcset="https://shieldcn.dev/badge/license-GPL_3.0.svg?variant=secondary&amp;mode=dark"><img alt="License" src="https://shieldcn.dev/badge/license-GPL_3.0.svg?variant=secondary&amp;mode=light"></picture></a>

Proton Mail Forwarder is a small mail server that forwards mail from your apps into your Proton Mail inbox like any other message, without Proton Mail Bridge and without a copy under Sent.

</div>

---

## Installation

Needs a paid Proton Mail plan with a custom domain, and an SMTP token for an address on it.

### Container

```yaml
services:
  forwarder:
    image: ivenos/proton_mail_forwarder:latest
    environment:
      PROTON_SMTP_USERNAME: hello@example.com
      PROTON_SMTP_TOKEN: your-smtp-token
    volumes:
      - ./data:/data
    restart: unless-stopped
```

```sh
docker compose up -d
docker compose exec forwarder proton_mail_forwarder login
```

### Without Docker

It listens on `127.0.0.1:2525` and keeps the session in `~/.config/proton_mail_forwarder`.

```sh
git clone https://github.com/ivenos/proton_mail_forwarder.git
cd proton_mail_forwarder
go build -o proton_mail_forwarder ./src
./proton_mail_forwarder login
PROTON_SMTP_USERNAME=hello@example.com PROTON_SMTP_TOKEN=... ./proton_mail_forwarder serve
```

### Signing in

- The login is needed to delete the copies under Sent, the SMTP token only sends.
- Two-factor login works with an authenticator app, not with a security key alone.
- `session.json` in the data folder can see subjects and addresses and delete mail, but not read messages. Keep it private.
- `proton_mail_forwarder logout` ends the session.

## Usage

Apps send to port 25 of the container, without TLS. Keep the port inside a Docker network.

- Messages show the original sender's name, replies go to the sender.
- Every message lands in your own inbox, under the recipient's address if it is one of yours.
- The copy under Sent is deleted for good. Failed deletions are retried hourly for a day.
- Mail that cannot be delivered right now gets a temporary error (451), mail Proton refuses a permanent one (554).
- `proton_mail_forwarder healthcheck` fails while mail cannot be delivered, a copy stays under Sent or the forwarder hangs.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `SMTP_LISTEN` | `:25` | Address and port to listen on |
| `SMTP_USERNAME` | - | Username the apps log in with, optional |
| `SMTP_PASSWORD` | - | Password for `SMTP_USERNAME` |
| `PROTON_SMTP_USERNAME` | - | Address of the SMTP token (required) |
| `PROTON_SMTP_TOKEN` | - | SMTP token (required) |
| `MAX_MESSAGE_SIZE` | `26214400` | Largest message in bytes, up to 36480000 |
| `DATA_DIR` | `/data` | Folder for the session and the state |

## License

Copyright © Iven Schlösser. Proton Mail Forwarder is free software, licensed under the [GNU General Public License v3.0 only](LICENSE). You may use, modify and redistribute it. Anyone distributing a modified version must release it under the same license and make its source code available.

Proton Mail Forwarder builds on [go-proton-api](https://github.com/ProtonMail/go-proton-api) and [gopenpgp](https://github.com/ProtonMail/gopenpgp) by Proton AG and on [go-smtp](https://github.com/emersion/go-smtp), each under the MIT License.

Proton Mail Forwarder is not affiliated with or endorsed by Proton AG. "Proton" and "Proton Mail" are trademarks of Proton AG.
