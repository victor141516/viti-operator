# SWu backend laboratory

This laboratory exercises a tunnel backend as an external process against a
controlled EAP-AKA ePDG. It intentionally sits below Asterisk and the product
clients so the same observable contract can be applied to the legacy Python
backend and the strongSwan backend.

The test subscriber and Milenage material in this directory are public,
non-production fixtures. Never add a real IMSI, K, OP, OPc or captured carrier
challenge here.

The initial milestone covers deterministic attach, lifecycle events, inner
address and P-CSCF assignment, route isolation, and real bidirectional UDP and
TCP transport. Fault injection and rekey scenarios are added to the same
Compose topology rather than special-cased in either backend.

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
