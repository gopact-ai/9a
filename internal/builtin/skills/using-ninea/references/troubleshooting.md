# Troubleshooting

Start with the user-facing state:

```sh
9a status
9a doctor
9a search <integration-or-capability>
```

Agents should add `--json` to `search` and `run`. For every failed `run`, read
`data.sideEffect` before taking another action:

- `none`: no request reached the upstream system; follow `nextAction` before
  retrying.
- `possible`: the upstream outcome is unknown; never retry automatically.
  Inspect upstream state when possible and report the uncertainty to the user.

`retryable` does not override `sideEffect`. An `approval_required` retry also
requires explicit approval for the exact same input and must use the returned
`data.approvalToken`. Tokens are single-use, expire after 10 minutes, and are
invalidated by a daemon restart. `approval_mismatch` means the input differs or
the token is invalid, expired, or already used; run without `--approve`, review
the new preflight, and ask again.

If a run fails with `capability_changed`, inspect the capability again. Any
earlier approval is invalid because the executable contract changed. If it
fails with `transport_error` and `sideEffect: possible`, the local runtime may
already be processing the request; do not retry automatically.

Common recovery actions:

- Invalid manifest: fix the exact field and line reported, then run `9a connect`
  again. A failed update does not replace the last working integration.
- Capability not found: run `9a search`, then use the displayed
  `<integration>/<capability>` reference.
- Gateway Skill changed or missing: explain that `9a doctor --fix` can also
  reconnect stale sources, which may start MCP code or contact A2A. Run it only
  after the user agrees.
- Missing credential: ask the user to run the exact `9a secret set` command
  shown by NineA. Never request or handle the secret value in the prompt.
- Integration no longer needed: run `9a disconnect <name>`. The source remains
  at `.9a/integrations/<name>.yaml`.
- Incompatible local state: follow the exact state path and move/remove action
  reported by NineA. Do not edit the database in place.

Follow the next action printed by the public commands. Do not invent recovery
steps outside that interface unless `9a doctor` reports that the local runtime
itself cannot start.
