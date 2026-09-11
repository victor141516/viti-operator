FROM fedora:43@sha256:a651ddf48ea28a06ed4e1e6519f51c9f47e7a5a138722ade87369b8fbb7e5b42

RUN dnf -y --disablerepo='*' --enablerepo=fedora install gcc git iproute iproute-tc python3 python3-devel python3-pip && \
    dnf clean all

RUN pip install --no-cache-dir \
    "https://files.pythonhosted.org/packages/source/p/pycryptodome/pycryptodome-3.23.0.tar.gz#sha256=447700a657182d60338bab09fdb27518f8856aecd80ae4c6bdddb67ff5da44ef" && \
    pip install --no-cache-dir \
    "git+https://github.com/mitshell/CryptoMobile.git@0857cbbf140c05c54688bdb2076c25eb8f3ddb89"

COPY engine/tunnel_contract.py /usr/local/bin/tunnel_contract.py
COPY test/swu-lab/aka_broker.py /opt/swu-lab/aka_broker.py
COPY test/swu-lab/lab_probe.py /opt/swu-lab/lab_probe.py
