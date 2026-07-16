# Errors and side effects

With `--json`, a failed command writes exactly one JSON error object:

```json
{
  "code": "approval_required",
  "error": "approval required to run weather/update",
  "data": {
    "integration": "weather",
    "capability": "update",
    "approvalToken": "v1.42.0123456789abcdef0123456789abcdef",
    "sideEffect": "none",
    "retryable": true,
    "nextAction": {
      "instruction": "Obtain explicit approval, then retry the exact same input with --approve v1.42.0123456789abcdef0123456789abcdef"
    }
  }
}
```

`code` is the stable machine-readable category. `error` is a human-readable
message and must not be parsed. `data` is optional and contains structured
context such as `call_id`, `retryable`, or `nextAction`.

Local argument and input-source failures use `invalid_request` and omit `data`
when there is no additional context.

## Approval retry

`approval_required` means NineA rejected the run before creating a call or
contacting the upstream system. An agent must preserve and review the same
input, obtain explicit approval for that action, and only then retry the same
command with `--approve <data.approvalToken>`. The token binds the current
capability revision and exact input. It is single-use, expires 10 minutes after
issuance, and is invalidated by a daemon restart. It must not construct a
replacement command that drops or changes input, nor infer approval from the
original request.

`approval_mismatch` also has `sideEffect: "none"`. It means the token is
malformed, expired, already used, invalidated by a daemon restart, or belongs to
different input; run again without `--approve`, review the new preflight, and
obtain explicit approval. Do not substitute the old token.

If the integration changes between inspection and execution, NineA returns
`capability_changed` with `sideEffect: "none"`. The prior approval no longer
applies: inspect the capability again and obtain approval for its current
contract before retrying.

## Transport uncertainty

`transport_error` means the CLI could not complete its exchange with the local
NineA runtime. When `sideEffect` is `none`, the request was not sent and the
runtime may be started safely before retrying. When it is `possible`, the local
runtime may already be processing the request; do not retry automatically.

If runtime startup reports that the state database is incompatible, follow its
path-specific instruction to move or remove that database before restarting.
NineA does not rewrite or silently migrate unknown state.

## `sideEffect`

When `data.sideEffect` is present on a `run` error, it has one of these values:

- `none` — NineA did not send a request upstream. Correct the request or follow
  `nextAction` before retrying.
- `possible` — the upstream system may have accepted all or part of the action,
  or its outcome is unknown. Inspect upstream state before deciding whether to
  retry.

`retryable` never overrides these rules. In particular, explicit approval is
still required for `approval_required`, and `possible` must never be retried
automatically.
