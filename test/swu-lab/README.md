# SWu backend laboratory

This laboratory exercises a tunnel backend as an external process against a
controlled EAP-AKA ePDG. It intentionally sits below Asterisk and the product
clients so the same observable contract can be applied to the legacy Python
backend and the strongSwan backend.

The test subscriber and Milenage material in this directory are public,
non-production fixtures. Never add a real IMSI, K, OP, OPc or captured carrier
challenge here.

The laboratory covers deterministic attach, lifecycle events, inner address
and P-CSCF assignment, route isolation, bidirectional transport (including
IPv6 fragmentation and a large TCP stream), authentication faults, NAT-T,
keepalive/DPD, rekeys and an operator-initiated deletion. Every scenario uses
the same Compose topology rather than special-casing either backend.

Run the external suite from the repository root:

```console
python test/swu-lab/run_lab.py --backend python
```

The runner uses the same lifecycle for `python` and `strongswan`: reset the
isolated topology, build it, attach the selected backend, inspect only its
public status/events/network namespace, send UDP and TCP through the tunnel,
stop the backend, and inspect its durable terminal state. The Python baseline
is stored in `baselines/python.json`. Its duplicate `tunnel_down` is a recorded
legacy expected failure, not behavior that the strongSwan backend may copy.

The default `all` scenario also drives the AKA broker through HTTP 503,
response timeout and AUTS synchronization-failure modes. It verifies failure
state externally and proves that a new backend generation attaches after the
provider returns. The protocol scenarios capture an RFC 3948 NAT-T keepalive,
observe a DPD INFORMATIONAL round trip, verify client-initiated CHILD and IKE
rekeys by their changed SPIs, exercise responder-initiated rekeys, and require
an exact lifecycle after an operator DELETE. Use `--scenario attach`,
`--scenario auth-faults`, or `--scenario protocol` to run a bounded subset.

The legacy Python backend rejects responder-initiated CHILD and IKE rekeys and
emits a duplicate `tunnel_down` during a clean local stop. These are explicit
expected failures in `baselines/python.json`; they remain mandatory passes for
the strongSwan backend.
