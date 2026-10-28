#!/bin/sh
# Starts the image without a session: it must listen, refuse mail for now and report itself unhealthy.
set -eu

image="${1:?usage: image_test.sh IMAGE}"
name="proton_mail_forwarder-test-$$"

cleanup() { docker rm -f "$name" > /dev/null 2>&1 || true; }
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    docker logs "$name" >&2 || true
    exit 1
}

version=$(docker run --rm "$image" version)
[ -n "$version" ] || fail "no version printed"
echo "version: $version"

docker run -d --name "$name" -e PROTON_SMTP_USERNAME=hello@example.com -e PROTON_SMTP_TOKEN=test "$image" > /dev/null

i=0
until docker exec "$name" sh -c 'grep -q . /data/status' 2> /dev/null; do
    i=$((i + 1))
    [ "$i" -lt 30 ] || fail "no status after 30 seconds"
    sleep 1
done

greeting=$(docker exec "$name" sh -c 'printf "QUIT\r\n" | nc -w 5 127.0.0.1 25' | head -n 1)
case "$greeting" in
    220*) echo "greeting: $greeting" ;;
    *) fail "unexpected greeting: $greeting" ;;
esac

reply=$(docker exec "$name" sh -c 'printf "HELO test\r\nMAIL FROM:<a@example.org>\r\nRCPT TO:<b@example.org>\r\nDATA\r\nSubject: test\r\n\r\nhello\r\n.\r\nQUIT\r\n" | nc -w 30 127.0.0.1 25' | grep '^451' || true)
[ -n "$reply" ] || fail "mail was not deferred"
echo "deferred: $reply"

if docker exec "$name" proton_mail_forwarder healthcheck 2> /dev/null; then
    fail "healthcheck passed without a session"
fi

echo "PASS"
