# OpenAI Responses `partial` Missing Retry Design

## Problem

An OpenAI-compatible Volcengine Ark endpoint can reject a Responses request with
HTTP 400, `error.code=MissingParameter`, and `error.param=partial`. A captured
production request ended with an assistant message followed by a custom tool
call and its output. The current proactive compatibility normalizer removes
`partial` from every message when the final input item is not itself an
assistant message, so the request fails.

The proactive rule has changed several times in the past. Broadening it again
would modify requests that currently succeed and risks reintroducing earlier
provider compatibility failures.

## Chosen Approach

Keep the existing first-attempt normalization unchanged. When all of the
following conditions hold, retry the same account once with `partial: true` on
the last assistant message:

- The account has Responses message partial compatibility enabled.
- The upstream response status is HTTP 400.
- The error explicitly identifies a missing `partial` parameter.
- The current request has an input array containing an assistant message whose
  `partial` value is not already `true`.
- This request has not already used the partial-missing retry.

The retry mutates only the last assistant message. It does not add a top-level
field, alter user or developer messages, switch accounts, or change the normal
first-attempt behavior.

## Request Flow

1. Build and send the request using the existing compatibility rules.
2. Preserve the existing conditional diagnostic logs when the upstream returns
   the targeted missing-partial error.
3. Produce a retry body by setting `input[lastAssistant].partial=true`.
4. Retry once against the same account.
5. If the retry also fails, use the existing upstream error handling without a
   third attempt.

## Failure Safety

- Invalid JSON returns a normalization error and does not retry.
- Requests without an assistant message do not retry.
- Requests whose last assistant message already has `partial: true` do not
  retry because the mutation would be a no-op.
- A dedicated boolean guard prevents retry loops.
- Existing billing, failover, and downstream error behavior remains unchanged.

## Tests

- Unit tests verify exact error matching and retry-body mutation.
- A forward-path regression test uses the captured structural pattern:
  assistant message, custom tool call, custom tool call output.
- The fake upstream returns the missing-partial error once and success once;
  assertions verify exactly two requests and `partial: true` only on the last
  assistant message in the second request.
- Existing partial compatibility tests and the full Go test suite must remain
  green.

## Deployment

Build an immutable `linux/amd64` image tagged with the implementation commit,
push it to the existing Aliyun registry, and deploy only to `47.254.20.208`.
The Hangzhou nodes remain unchanged.
