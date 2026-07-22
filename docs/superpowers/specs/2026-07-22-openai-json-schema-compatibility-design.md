# OpenAI JSON Schema Compatibility Design

## Goal

Allow individual OpenAI API-key accounts whose upstream models reject
`json_schema` to downgrade structured-output requests to `json_object` without
changing the behavior of other accounts.

The initial users are the Volcano Ark endpoints backed by
`deepseek-v4-flash-260425` and `deepseek-v4-pro-260425`. Direct upstream tests
confirmed that both models accept `json_object` and reject `json_schema` on
both Chat Completions and Responses APIs.

## Account Configuration

Store an account-level setting in the existing `accounts.extra` JSONB column:

```json
{
  "openai_json_schema_mode": "force_json_object"
}
```

Supported values:

- `auto`: preserve the incoming request. This is the default when the key is
  absent or invalid.
- `passthrough`: explicitly preserve the incoming request.
- `force_json_object`: downgrade `json_schema` to `json_object`.

No database migration is required. The new key must be included in the
scheduler's compact account metadata so every request-serving node can read it.

The setting is shown only for OpenAI API-key accounts in the create, edit, and
bulk-edit account forms. The UI label is "JSON Schema 兼容模式" with choices
"自动", "原生透传", and "强制 JSON Object".

## Request Transformation

Apply the transformation only after an account has been selected and model
mapping has resolved the upstream model. This keeps the behavior scoped to the
chosen account rather than the requested model name or group.

For Chat Completions requests:

```text
response_format.type = json_schema
    -> response_format = {"type":"json_object"}
```

For Responses requests:

```text
text.format.type = json_schema
    -> text.format = {"type":"json_object"}
```

The conversion applies to both raw Chat Completions forwarding and native
Responses forwarding, including Chat-to-Responses bridge paths. Requests with
another format, malformed format data, or an account not in
`force_json_object` mode remain unchanged.

## Schema Hint Preservation

When downgrading, serialize the original JSON Schema into a short additional
instruction asking the upstream model to return only a JSON object matching
that schema.

- Responses requests append the hint to `instructions`, preserving existing
  instructions.
- Chat Completions requests append a system message rather than changing a user
  message.

The hint improves field-name and type adherence but does not create a strict
guarantee. The gateway will not buffer streaming responses for local schema
validation because doing so would remove streaming behavior and increase
time-to-first-token.

## Error Handling and Observability

Transformation failures caused by malformed JSON must leave the original body
unchanged and return a local serialization error only when a partial rewrite
would otherwise be sent.

Emit a structured debug log when a downgrade occurs containing account ID,
upstream model, and request protocol. Do not log the schema or request content.

## Compatibility

- Existing accounts default to `auto`, so deployment alone changes no traffic.
- OAuth accounts and non-OpenAI accounts do not expose or apply this setting.
- `passthrough` exists as an explicit override for troubleshooting and future
  upstream upgrades.
- Hangzhou and US deployments use independent PostgreSQL databases, so the
  setting must be selected separately in each environment.

## Verification

Add unit and integration-level tests covering:

1. Chat `json_schema` downgrade with schema hint preservation.
2. Responses `json_schema` downgrade with schema hint preservation.
3. Chat-to-Responses conversion paths.
4. Streaming requests remain streaming.
5. `auto`, `passthrough`, OAuth, and non-target formats are unchanged.
6. Missing or invalid account metadata safely defaults to `auto`.
7. Frontend create, edit, and bulk-edit forms persist the selected value.
8. Scheduler cache retains the new account metadata key.

