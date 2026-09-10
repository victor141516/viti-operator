# SWu backend laboratory

This laboratory exercises a tunnel backend as an external process against a
controlled EAP-AKA ePDG. It intentionally sits below Asterisk and the product
clients so the same observable contract can be applied to the legacy Python
backend and the strongSwan backend.

The test subscriber and Milenage material in this directory are public,
non-production fixtures. Never add a real IMSI, K, OP, OPc or captured carrier
challenge here.

The initial milestone covers deterministic attach, lifecycle events, inner
address and P-CSCF assignment. Transport, fault injection and rekey scenarios
are added to the same Compose topology rather than special-cased in either
backend.
