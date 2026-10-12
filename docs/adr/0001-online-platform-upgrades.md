# Stable listener ownership inside rolling upgrades

Rolling upgrades replace replicas one wave at a time and rely on peers to
carry traffic, so a sole replica was briefly unavailable. Keep network serving
ownership outside replaceable application processes for the control plane,
console, registry and Envoy, and hand a replica's sockets to its new process in
place. This gives one transition interface to every serving role without a
second database or a duplicated platform on a single host.

The old generation is then drained through the same stop preparation and
SIGTERM handling every stop uses, rather than waiting for clients to close.
Waiting would keep an upgrade pending indefinitely behind agent sessions, xDS
streams and keep-alive connections. Draining bounds the overlap; clients
reconnect to the new generation and no request is replayed.

Schema evolution stays with release-supplied expand, backfill and contract hooks
and the database's `min_compatible_version` boundary. There is no separate
maintenance mode: adopting the owner, changing its executable or changing
listener addresses restarts the owner through its drained stop, and the plan
reports each sole replica that restarts.
