FROM fedora:43@sha256:a651ddf48ea28a06ed4e1e6519f51c9f47e7a5a138722ade87369b8fbb7e5b42

ARG STRONGSWAN_COMMIT=b43f6bfeed34b4afc8a7eb0c0de38e6e70177847

RUN dnf -y install autoconf automake bison flex gcc git gperf iproute iproute-tc \
    libtool make openssl-devel pkg-config procps-ng python3 tcpdump && dnf clean all

RUN git init /usr/src/strongswan && \
    cd /usr/src/strongswan && \
    git remote add origin https://github.com/strongswan/strongswan.git && \
    git fetch --depth 1 origin "${STRONGSWAN_COMMIT}" && \
    git checkout --detach FETCH_HEAD && \
    ./autogen.sh && \
    ./configure --prefix=/usr --sysconfdir=/etc \
      --enable-swanctl --enable-vici --enable-openssl \
      --enable-aes --enable-sha1 --enable-sha2 --enable-hmac \
      --enable-kernel-libipsec --enable-eap-identity --enable-eap-aka \
      --enable-eap-aka-3gpp --enable-p-cscf && \
    make -j"$(nproc)" && make install && ldconfig && \
    rm -rf /usr/src/strongswan

COPY test/swu-lab/epdg/strongswan.conf /etc/strongswan.conf
COPY test/swu-lab/epdg/swanctl.conf /etc/swanctl/swanctl.conf
COPY test/swu-lab/epdg/entrypoint.sh /entrypoint.sh
COPY test/swu-lab/echo_server.py /opt/swu-lab/echo_server.py

RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
