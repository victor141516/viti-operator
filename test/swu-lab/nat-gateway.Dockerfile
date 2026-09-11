FROM fedora:43@sha256:a651ddf48ea28a06ed4e1e6519f51c9f47e7a5a138722ade87369b8fbb7e5b42

RUN dnf -y --disablerepo='*' --enablerepo=fedora install iproute iptables && \
    dnf clean all

COPY test/swu-lab/nat-entrypoint.sh /usr/local/bin/nat-entrypoint.sh

RUN chmod +x /usr/local/bin/nat-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/nat-entrypoint.sh"]
