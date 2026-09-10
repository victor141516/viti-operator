FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk add --no-cache iptables

COPY test/swu-lab/nat-entrypoint.sh /usr/local/bin/nat-entrypoint.sh

RUN chmod +x /usr/local/bin/nat-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/nat-entrypoint.sh"]
