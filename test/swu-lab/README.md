# strongSwan SWu laboratory

This deterministic laboratory exercises the production SWu tunnel stack as an
external process against a controlled EAP-AKA ePDG. The supported backend is
strongSwan `charon` with `kernel-libipsec`; ESP is processed in userspace and
carried through a TUN interface. There is no alternate custom IKE/ESP backend.

The test subscriber and Milenage material in this directory are public,
non-production fixtures. Never add a real IMSI, K, OP, OPc, or captured carrier
challenge here.

The laboratory covers attach, lifecycle events, inner-address and P-CSCF
assignment, source-specific route isolation, bidirectional UDP/TCP transport,
IPv6 fragmentation, authentication faults, AUTS, NAT-T, keepalive/DPD, client-
and responder-initiated rekeys, reauthentication, colliding rekeys, operator
DELETE, process loss, and generation-safe recovery.

The AKA broker and backend live on a client-side network while the ePDG lives
on a separate operator-side network. A dedicated health-checked gateway routes
and masquerades traffic between them. NAT detection and RFC 3948 keepalive
assertions therefore exercise a translated UDP flow rather than forced
encapsulation on one Docker bridge.

Run the complete carrier-free suite from the repository root:

```console
python test/swu-lab/run_lab.py
```

Run a bounded subset with one of:

```console
python test/swu-lab/run_lab.py --scenario attach
python test/swu-lab/run_lab.py --scenario auth-faults
python test/swu-lab/run_lab.py --scenario protocol
python test/swu-lab/run_lab.py --scenario resilience
```

To verify a separately built production artifact, pass its tag:

```console
python test/swu-lab/run_lab.py --backend-image viti-operator-engine:local
```

Each scenario resets the isolated topology, observes the public status/event
contract and real network behavior, and verifies terminal lifecycle state. The
backend control FIFO accepts the newline-delimited commands `stop`, `reauth`,
`rekey-child`, and `rekey-ike`.
