# Repository guidance for coding agents

## Product and language

- Viti Operator is a self-hosted calling and SMS service that uses a physical
  SIM and the carrier's Wi-Fi Calling path.
- Keep user-facing copy, code comments, commit messages, issue/PR text, and
  documentation in English.
- Do not commit credentials, subscriber identifiers, phone numbers, recordings,
  packet captures, diagnostic dumps, APKs, or workstation-specific settings.

## Carrier tunnel architecture

- strongSwan `charon` is the only SWu/IKEv2 backend. It uses
  `kernel-libipsec`, `/dev/net/tun`, and the `ipsec0` userspace ESP interface.
- Do not introduce an alternate SWu backend or a runtime selector. Python
  remains valid for orchestration and adapters; it must not implement IKEv2 or
  ESP.
- Keep strongSwan and Asterisk in the same engine network namespace. Only
  traffic sourced from the carrier-assigned inner address may use table 220.
- Preserve the public tunnel contract in `engine/tunnel_contract.py`: atomic
  status plus ordered, durable `tunnel_start`, `tunnel_up`, and `tunnel_down`
  events for each generation.
- Treat published `DOWN` as a completion boundary: its `tunnel_down` event must
  already be durable, and production PID 1 must also wait for the exact adapter
  process to exit before leaving the network namespace.
- Losing an AKA provider must not proactively destroy an established tunnel.
  Recovery must not leave duplicate `charon` processes, stale route rules, or
  a stale `CONNECTED` status.
- P-CSCF and inner-address reconciliation must be transactional. Commit their
  applied state together in `tunnel.applied.json`, and do not advance it until
  rendering, restart/reload, and IMS registration actions have all succeeded.

## Verification

Run focused tests while editing, then verify the production image and isolated
SWu laboratory before merging tunnel changes:

```sh
(cd engine && python -m unittest test_strongswan_adapter.py test_tunnel_reconcile.py)
python test/test_swu_lab_runner.py
docker build -t viti-operator-engine:local -f engine/Dockerfile engine
python test/swu-lab/run_lab.py --backend-image viti-operator-engine:local --no-build
```

The packaged laboratory must exercise the real `/entrypoint.sh`, cancellation
during a blocked startup `IKE_AUTH`, in-process `charon` crash recovery, real
UDP/TCP transport, route isolation, and terminal shutdown state. Keep live
carrier tests opt-in; they are user acceptance tests, not part of the
deterministic laboratory.

For unrelated application changes, use the narrower commands documented in
`README.md` and `docs/OPERATIONS.md`. Update architecture and operations
documentation only after behavior and tests are stable.
