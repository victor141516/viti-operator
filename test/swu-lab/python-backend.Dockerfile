FROM fedora:40

RUN dnf -y install gcc git iproute net-tools procps-ng python3 python3-devel python3-pip python3-pyscard && \
    dnf clean all

RUN pip install --no-cache-dir pyserial pycryptodome cryptography requests \
    "git+https://github.com/mitshell/CryptoMobile.git" \
    "git+https://github.com/mitshell/card.git"

COPY engine/swu_ike.py /usr/local/bin/swu_ike.py
COPY engine/tunnel_backend.py /usr/local/bin/tunnel_backend.py
COPY engine/tunnel_contract.py /usr/local/bin/tunnel_contract.py
COPY engine/event_outbox.py /usr/local/bin/event_outbox.py
COPY test/swu-lab/aka_broker.py /opt/swu-lab/aka_broker.py
COPY test/swu-lab/lab_probe.py /opt/swu-lab/lab_probe.py

ENTRYPOINT ["python3", "/usr/local/bin/tunnel_backend.py"]
