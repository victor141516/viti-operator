# VoWiFi engine image: strongSwan SWu tunnel + sysmocom Asterisk + USIM bridge.
# Based on phcoder/asterisk-docker (osmocom "VoWiFi with Asterisk"), with additions:
#   - strongSwan    : charon + kernel-libipsec userspace ESP, controlled through VICI
#   - pin_keeper.py : holds SIM PIN (CHV1) verified so IKE EAP-AKA + SIP IMS-AKA work on PIN-enabled SIMs
#   - ami_usim.py   : reference USIM<->AMI bridge, patched to VERIFY PIN + report status
#   - render.py     : renders /config/instance.json -> Asterisk configs at container start
#
# Reproducibility: immutable base/source revisions and downloaded-source checksums
# keep the native telecom stack reviewable across clean builds.
FROM fedora:43@sha256:a651ddf48ea28a06ed4e1e6519f51c9f47e7a5a138722ade87369b8fbb7e5b42

# pcsc-lite is pinned to a fixed upstream version and built from source (see below), so the
# engine's PC/SC client library matches the host pcscd EXACTLY across any distro — this is the
# same PCSC_VERSION the installer pins on the host and the control image uses. Avoids the
# client/server protocol mismatch that distro-default pcsc-lite packages cause.
ARG PCSC_VERSION=2.3.3
ARG PCSC_SHA256=00b667aa71504ed1d39a48ad377de048c70dbe47229e8c48a3239ab62979c70f
ARG STRONGSWAN_COMMIT=b43f6bfeed34b4afc8a7eb0c0de38e6e70177847
ARG PJPROJECT_COMMIT=20537ab1e6beb2baf9ec15599e0217a601cf7ce5
ARG ASTERISK_COMMIT=d231cb2c658545773fcd5eb9de787219b9ef6566

WORKDIR /home/asterisk-build

RUN dnf -y install git gcc g++ openssl-devel patch libedit-devel opus-devel libuuid-devel autoconf \
    automake libtool gettext perl python3 flex bison pkg-config m4 gettext-devel gperf gmp-devel \
    jansson-devel libxml2-devel sqlite-devel opencore-amr-devel vo-amrwbenc-devel \
    libmnl-devel speex-devel speexdsp-devel libvorbis-devel alsa-lib-devel portaudio-devel \
    libcurl-devel xmlstarlet postgresql-devel unixODBC-devel neon-devel gmime-devel lua-devel \
    uriparser-devel libxslt-devel mysql-devel bluez-libs-devel radcli-devel freetds-devel \
    jack-audio-connection-kit-devel libcap-devel net-snmp-devel corosynclib-devel newt-devel \
    popt-devel libical-devel spandsp-devel openldap-devel codec2-devel fftw-devel libsndfile-devel \
    unbound-devel wget subversion libresample-devel binutils-devel libsrtp-devel gsm-devel iproute \
    procps-ng net-tools python3-pysocks python3-requests python3-pyscard python3-jinja2 \
    python3-build python3-devel python3-pip json-c-devel diffutils \
    meson ninja-build perl-podlators systemd-devel \
    tcpdump bind-utils iputils

# Build pcsc-lite ${PCSC_VERSION} from source (client library + headers). Installs over any
# distro-provided libpcsclite pulled in by python3-pyscard so pyscard (loads libpcsclite.so.1 at
# runtime) uses the pinned version and talks the same protocol as the host pcscd.
RUN cd /tmp && \
    wget -qO pcsc.tar.gz "https://github.com/LudovicRousseau/PCSC/archive/refs/tags/${PCSC_VERSION}.tar.gz" && \
    echo "${PCSC_SHA256}  pcsc.tar.gz" | sha256sum -c - && \
    tar xf pcsc.tar.gz && cd "PCSC-${PCSC_VERSION}" && \
    meson setup builddir --prefix=/usr -Dpolkit=false && \
    ninja -C builddir && ninja -C builddir install && \
    ldconfig && \
    rm -rf /tmp/pcsc.tar.gz "/tmp/PCSC-${PCSC_VERSION}"

# sysmocom pjproject + Asterisk (VoLTE/VoWiFi/IMS-AKA/SMS-over-IMS fork), identical to reference build
COPY patches/asterisk/ /home/asterisk-build/patches/asterisk/
RUN git init pjproject && cd pjproject && \
    git remote add origin https://gitea.sysmocom.de/sysmocom/pjproject.git && \
    git fetch --depth=1 origin "${PJPROJECT_COMMIT}" && git checkout --detach FETCH_HEAD && \
    cd .. && git init asterisk && cd asterisk && \
    git remote add origin https://gitea.sysmocom.de/sysmocom/asterisk.git && \
    git fetch --depth=1 origin "${ASTERISK_COMMIT}" && git checkout --detach FETCH_HEAD && \
    git config user.email "build@vowifi" && git config user.name "vowifi build" && \
    git fetch origin f1b60dcd9568c4045512fd0d8b619b9fb91a7f35 && \
    git cherry-pick f1b60dcd9568c4045512fd0d8b619b9fb91a7f35 && \
    for p in /home/asterisk-build/patches/asterisk/*.py; do echo "applying $p"; python3 "$p" || exit 1; done && \
    ln -s ../../../pjproject third-party/pjproject/source && \
    sh bootstrap.sh && \
    ./configure --enable-binary-modules && \
    make menuselect/menuselect menuselect-tree menuselect.makeopts && \
    ./menuselect/menuselect --enable codec_opus --disable BUILD_NATIVE && \
    make -j$(nproc) && \
    make install

RUN pip install --no-cache-dir panoramisk==1.4

# Build the pinned userspace-IPsec backend. kernel-libipsec owns ESP and creates
# ipsec0; kernel-netlink is loaded only as the Linux network/route provider. The
# modular strongSwan config selects kernel-libipsec before kernel-netlink and the
# black-box lab asserts that no kernel XFRM state or policy is installed.
COPY patches/strongswan/0001-configurable-message-id-sync-advertisement.patch /tmp/strongswan-message-id-sync.patch
COPY patches/strongswan/0002-configurable-request-idr.patch /tmp/strongswan-request-idr.patch
RUN git init /usr/src/strongswan && \
    cd /usr/src/strongswan && \
    git remote add origin https://github.com/strongswan/strongswan.git && \
    git fetch --depth 1 origin "${STRONGSWAN_COMMIT}" && \
    git checkout --detach FETCH_HEAD && \
    git apply /tmp/strongswan-message-id-sync.patch && \
    git apply --recount /tmp/strongswan-request-idr.patch && \
    ./autogen.sh && \
    ./configure --prefix=/usr --sysconfdir=/etc \
      --enable-swanctl --enable-vici --enable-python-wheels --enable-openssl \
      --enable-aes --enable-sha1 --enable-sha2 --enable-hmac \
      --enable-kernel-libipsec --enable-eap-identity --enable-eap-aka && \
    make -j"$(nproc)" && make install && ldconfig && \
    python3 -m pip install --no-cache-dir ./src/libcharon/plugins/vici/python

COPY strongswan/eap_aka_broker_plugin.c /tmp/eap_aka_broker_plugin.c
RUN cd /usr/src/strongswan && \
    gcc -std=gnu11 -fPIC -shared -include config.h \
      -I. -Isrc/libstrongswan -Isrc/libcharon -Isrc/libsimaka \
      $(pkg-config --cflags libcurl json-c) \
      /tmp/eap_aka_broker_plugin.c \
      -L/usr/lib64/ipsec -Wl,-rpath,/usr/lib64/ipsec \
      -lsimaka -lcharon -lstrongswan $(pkg-config --libs libcurl json-c) -pthread \
      -o /usr/lib64/ipsec/plugins/libstrongswan-eap-aka-broker.so && \
    cd / && rm -rf /usr/src/strongswan /tmp/eap_aka_broker_plugin.c /tmp/strongswan-message-id-sync.patch /tmp/strongswan-request-idr.patch && \
    test -x /usr/libexec/ipsec/charon && \
    test -f /usr/lib64/ipsec/plugins/libstrongswan-kernel-libipsec.so && \
    test -f /usr/lib64/ipsec/plugins/libstrongswan-eap-aka-broker.so && \
    ! ldd /usr/lib64/ipsec/plugins/libstrongswan-eap-aka-broker.so | grep -q 'not found' && \
    python3 -c 'import vici'

# our scripts + templates
COPY pin_keeper.py   /usr/local/bin/pin_keeper.py
COPY ami_usim.py     /usr/local/bin/ami_usim.py
COPY tunnel_contract.py /usr/local/bin/tunnel_contract.py
COPY tunnel_reconcile.py /usr/local/bin/tunnel_reconcile.py
COPY strongswan_adapter.py /usr/local/bin/strongswan_adapter.py
COPY strongswan/strongswan.conf /etc/strongswan.conf
COPY render.py       /usr/local/bin/render.py
COPY notify.py       /usr/local/bin/notify.py
COPY event_outbox.py /usr/local/bin/event_outbox.py
COPY recording_enabled.py /usr/local/bin/recording_enabled.py
COPY test_ami_usim_http.py /usr/local/bin/test_ami_usim_http.py
COPY test_tunnel_contract.py /usr/local/bin/test_tunnel_contract.py
COPY test_tunnel_reconcile.py /usr/local/bin/test_tunnel_reconcile.py
COPY test_strongswan_adapter.py /usr/local/bin/test_strongswan_adapter.py
COPY test_event_outbox.py /usr/local/bin/test_event_outbox.py
COPY test_notify.py /usr/local/bin/test_notify.py
COPY templates/      /opt/vowifi/templates/
COPY entrypoint.sh   /entrypoint.sh
RUN cd /usr/local/bin && python3 -m unittest test_ami_usim_http.py test_tunnel_contract.py test_tunnel_reconcile.py test_strongswan_adapter.py test_event_outbox.py test_notify.py && \
    rm /usr/local/bin/test_ami_usim_http.py /usr/local/bin/test_tunnel_contract.py /usr/local/bin/test_tunnel_reconcile.py /usr/local/bin/test_strongswan_adapter.py /usr/local/bin/test_event_outbox.py /usr/local/bin/test_notify.py && \
    chmod +x /usr/local/bin/*.py /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
