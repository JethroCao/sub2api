# Seedance 2.5 Model Support Design

## Goal

Expose `seedance-2.5` as an exact, model-keyed public video model backed by a
configured Ark endpoint, without widening Seedance provider defaults or
advertising operations that the current adapter cannot execute.

## Official contract

The implementation is based on the Volcano Ark model list and Seedance 2.5
tutorial published on 2026-08-11:

- Model ID: `doubao-seedance-2-5-260628`.
- Supported upstream modes include text-to-video, first-frame and
  first-and-last-frame generation, multimodal image/video/audio references,
  video editing, video extension, and generated audio.
- Output constraints are 480p or 720p, 24 fps, and 4–30 seconds.
- Ratios are `21:9`, `16:9`, `4:3`, `1:1`, `3:4`, `9:16`, plus `adaptive`
  where accepted by the API.
- Offline inference is not supported.

Sources:

- https://docs.volcengine.com/docs/82379/1330310?lang=zh
- https://docs.volcengine.com/docs/82379/2607688?lang=zh

## Supported gateway scope

The current Seedance provider adapter only submits the canonical `generation`
operation, and the canonical public request has no audio-reference asset type.
This change therefore advertises only the subset the gateway can faithfully
execute:

- Public model name: `seedance-2.5`.
- Operation: `generation` only.
- Inputs: text, first frame, first and last frame, reference images, and
  reference videos.
- Generated audio: supported through the existing boolean audio field.
- Duration: 4–30 seconds.
- Resolution: 480p or 720p.
- Aspect ratio: `21:9`, `16:9`, `4:3`, `1:1`, `3:4`, `9:16`, or `adaptive`.

The catalog will not advertise last-frame-only generation, audio-reference
assets, editing, or extension. Those require separate canonical request and
adapter work.

## Design

Add a second exact `provider + model` entry to the production
`VideoCapabilityCatalog`. Do not add a provider-wide Seedance fallback. Account
model mapping remains administrator-configured, for example:

```text
seedance-2.5 -> ep-xxxxxxxx
```

The adapter will continue replacing the public model with the configured Ark
endpoint ID before submission. Pricing remains explicit per group and model,
so enabling an account mapping alone does not silently enable unpriced use.

## Safety and compatibility

- Existing `seedance-2.0` behavior is unchanged.
- Unknown Seedance model names continue to fail closed.
- `seedance-2.5` requests outside the documented duration, resolution, or
  ratio constraints fail before account selection and upstream submission.
- Edit, extension, last-frame-only, and unsupported resolution requests remain
  rejected as unsupported capabilities.
- No database migration is required; model mappings and pricing rules already
  store arbitrary exact external model names.

## Verification

Use test-driven development to add production-catalog tests that prove:

1. A valid `seedance-2.5` generation request is accepted.
2. The lower and upper duration boundaries are accepted and out-of-range
   values are rejected.
3. 480p/720p and all supported ratios are accepted; 1080p is rejected.
4. First-frame, first-and-last-frame, reference image/video, and generated
   audio requests are accepted.
5. Last-frame-only, edit, extension, and unknown Seedance models are rejected.
6. Existing `seedance-2.0` production-catalog tests continue to pass.
