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

The backend and AKA broker live on a client-side network while the ePDG lives
on a separate operator-side network. A dedicated, health-checked gateway
routes and masquerades traffic between them. Consequently NAT detection and
RFC 3948 keepalive assertions exercise a real translated UDP flow instead of
relying only on forced encapsulation between peers on one Docker bridge.

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

`--scenario resilience` performs a complete EAP-AKA reauthentication, removes
the AKA provider both while the existing SA is idle and during a forced
reauthentication, collides client- and responder-initiated CHILD and IKE
rekeys under deterministic `netem` delay, then kills and restarts the backend
while preserving its runtime volume. It verifies generation isolation and
real transport after recovery. The backend control FIFO accepts the stable
newline-delimited commands `stop`, `reauth`, `rekey-child`, and `rekey-ike`;
the legacy one-letter commands remain aliases.

The legacy Python backend rejects responder-initiated CHILD and IKE rekeys and
emits a duplicate `tunnel_down` during a clean local stop. These are explicit
expected failures in `baselines/python.json`; they remain mandatory passes for
the strongSwan backend. The resilience baseline additionally records Python's
duplicate `tunnel_up` after reauthentication, destruction of a valid SA after
a failed reauthentication, CHILD-rekey collision transport failure,
nondeterministic IKE-rekey collision transport failure, and missing
`tunnel_down` after an uncatchable process crash. New-generation recovery
remains required behavior.
