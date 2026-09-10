FROM fedora:40

ARG STRONGSWAN_COMMIT=b43f6bfeed34b4afc8a7eb0c0de38e6e70177847

RUN dnf -y install autoconf automake bison curl-devel diffutils flex gcc git gperf patch \
    iproute iproute-tc json-c-devel libtool make openssl-devel pkg-config \
    procps-ng python3 python3-build python3-devel python3-pip tcpdump && dnf clean all

COPY engine/patches/strongswan/0001-configurable-message-id-sync-advertisement.patch /tmp/strongswan-message-id-sync.patch

RUN git init /usr/src/strongswan && \
    cd /usr/src/strongswan && \
    git remote add origin https://github.com/strongswan/strongswan.git && \
    git fetch --depth 1 origin "${STRONGSWAN_COMMIT}" && \
    git checkout --detach FETCH_HEAD && \
    patch -p1 < /tmp/strongswan-message-id-sync.patch && \
    ./autogen.sh && \
    ./configure --prefix=/usr --sysconfdir=/etc \
      --enable-swanctl --enable-vici --enable-python-wheels --enable-openssl \
      --enable-aes --enable-sha1 --enable-sha2 --enable-hmac \
      --enable-kernel-libipsec --enable-eap-identity --enable-eap-aka && \
    make -j"$(nproc)" && make install && ldconfig && \
    python3 -m pip install --no-cache-dir ./src/libcharon/plugins/vici/python

COPY engine/strongswan/eap_aka_broker_plugin.c /tmp/eap_aka_broker_plugin.c

RUN cd /usr/src/strongswan && \
    gcc -std=gnu11 -fPIC -shared -include config.h \
      -I. -Isrc/libstrongswan -Isrc/libcharon -Isrc/libsimaka \
      $(pkg-config --cflags libcurl json-c) \
      /tmp/eap_aka_broker_plugin.c \
      -L/usr/lib64/ipsec -Wl,-rpath,/usr/lib64/ipsec \
      -lsimaka -lcharon -lstrongswan $(pkg-config --libs libcurl json-c) -pthread \
      -o /usr/lib64/ipsec/plugins/libstrongswan-eap-aka-broker.so && \
    rm -rf /usr/src/strongswan /tmp/eap_aka_broker_plugin.c /tmp/strongswan-message-id-sync.patch

COPY engine/strongswan/strongswan.conf /etc/strongswan.conf
COPY engine/strongswan_adapter.py /usr/local/bin/strongswan_adapter.py
COPY engine/tunnel_backend.py /usr/local/bin/tunnel_backend.py
COPY engine/tunnel_contract.py /usr/local/bin/tunnel_contract.py
COPY engine/event_outbox.py /usr/local/bin/event_outbox.py
COPY test/swu-lab/lab_probe.py /opt/swu-lab/lab_probe.py
COPY test/swu-lab/backend-entrypoint.sh /usr/local/bin/backend-entrypoint.sh

RUN chmod +x /usr/local/bin/strongswan_adapter.py /usr/local/bin/backend-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/backend-entrypoint.sh"]
