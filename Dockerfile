FROM alpine:3.24.3

ARG TARGETARCH
COPY --chmod=755 dist/proton_mail_forwarder-linux-${TARGETARCH} /usr/local/bin/proton_mail_forwarder
COPY LICENSE dist/THIRD_PARTY_NOTICES /usr/share/licenses/proton_mail_forwarder/

ENV SMTP_LISTEN=:25 DATA_DIR=/data
WORKDIR /data
EXPOSE 25

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD ["proton_mail_forwarder", "healthcheck"]

ENTRYPOINT ["proton_mail_forwarder"]
CMD ["serve"]
