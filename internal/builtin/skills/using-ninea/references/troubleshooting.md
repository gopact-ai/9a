# Lifecycle and Troubleshooting

```sh
9a attach
9a status --json
9a update --check
9a update
9a detach
```

`attach` chooses FUSE when available and otherwise records a visible fallback
to read-only managed files. `update` rediscovers providers, refreshes the
Catalog, upgrades the built-in Skill, repairs valid owned directory
projections, and removes stale views. `detach` removes only the current
workspace view; it preserves providers, API sources, ACLs, and call history.
Because update rediscovers providers and changes the shared Catalog, it requires
an administrator token.

## Upgrade the software

Do not confuse a software upgrade with a workspace update:

```sh
brew update
brew upgrade gopact-ai/tap/ninea
# Restart ninead with the same state database and socket, without the bootstrap
# token, then:
9a update --check
9a update
9a status --json
```

`brew upgrade` replaces the `9a` and `ninead` binaries. `9a update` upgrades the
built-in Skill and reconciles managed views; it does not install software. Get
the user's approval before changing packages or restarting `ninead`. Preserve
the state database and back it up when it contains important configuration.
Use `9a update --all` only when every attached workspace should be reconciled.

Common checks:

- Cannot connect: verify `ninead` and `NINEA_SOCKET`.
- Unauthorized: set `NINEA_TOKEN` to an issued identity token.
- Empty search: grant `read` on the capability.
- Invocation denied: grant `invoke` separately.
- FUSE fallback: inspect `9a status --json`; install/enable the platform FUSE
  runtime or explicitly use the directory backend.
- Changed content or modes: run `9a update`; do not edit the generated
  directory. If the ownership manifest itself is missing or corrupt, move the
  directory aside before updating so 9A never deletes ambiguous content.
- Projection conflict: move the user-owned directory. 9A never overwrites it.
- Missing provider credentials: restart `ninead` from an environment containing
  them; projected files never contain resolved secrets.

For long-running work:

```sh
CALL_ID="$(printf '%s\n' '<json>' | 9a calls start <capability-id>)"
9a calls get "$CALL_ID"
9a calls events "$CALL_ID" --after 0 --limit 100
9a calls cancel "$CALL_ID"
```
