# Sail — work that follows Scout v0.6.0

Sail ships no Agent Studio or agent-conversation UI today, so the first two items are new
horizontal components rather than migrations. The wire contracts are Scout's:
[STUDIO_API.md](STUDIO_API.md) for Studio, `domain.TurnEvent` for the turn stream. Sail conventions
apply: signal `input()`/`output()`, native control flow, field-level `inject()`, templates in their
own `*.html`, no `.css`/`.scss`, backend values never defaulted on the client.

| ID | Pri | Item | Depends on |
|---|---|---|---|
| SAIL-A0 | P1 | Secondary entry point `@nauticana/sail/agent` | — |
| SAIL-A1 | P1 | `studio-v2` models and `AgentStudioService` | SAIL-A0 |
| SAIL-A2 | P1 | Prompt layer editor | SAIL-A1 |
| SAIL-A3 | P2 | Release section provenance | SAIL-A1 |
| SAIL-A4 | P1 | Typed turn events, including `effect` | SAIL-A0 |
| SAIL-A5 | P2 | Reply stream resume and turn cancellation | SAIL-A4 |
| SAIL-A6 | P3 | Decision chain timeline | SAIL-A0 |

## SAIL-A0 — secondary entry point

Sail is published as source (`"main": "src/index.ts"`), so everything the root barrel exports is
type-checked in every consumer's build, including apps that do not use Scout. Everything below
therefore lives under `src/agent/` with its own `src/agent/index.ts` and is **not** exported from
`src/index.ts`.

```json
"exports": {
  ".":       "./src/index.ts",
  "./agent": "./src/agent/index.ts"
},
"sideEffects": ["./src/model/decorator.ts"]
```

- An app without Scout never resolves `src/agent/`: no compile cost, no bundle cost, no build it can break.
- `src/agent/` adds no peer dependency; Angular, Material, and rxjs are already required.
- `sideEffects` cannot be a blanket `false`: `model/decorator.ts` registers class-validator
  constraints when it is evaluated. List every file that does; the rest become safely tree-shakable.
- `exports` closes deep imports such as `@nauticana/sail/service/billing.service`, and consumers use
  them today. Either export each deep path in use (`"./service/*": "./src/service/*.ts"`,
  `"./model/*": "./src/model/*.ts"`) or make it a clean break with the old→new mapping in
  `migration_guide.json`; do not add `exports` without deciding which.
- `npm run check` must compile `src/agent/index.ts` too, since no root import reaches it.

## SAIL-A1 — `studio-v2` models and service

- `agent/model/agent_studio.ts`: `AgentSummary`, `AgentDraft`, `AgentLanguageDraft`, `AgentPromptSection`,
  `AgentPromptLayer`, `AgentRelease`, `AgentReleaseSection` with `source`, `AgentResetRequest`.
  Field names are the JSON names in `api/studio.go`; apps re-export them through their own model barrels.
- A prompt section is `layers[]` plus `effective_text`/`effective_output`. There is no
  `business_*`, `default_*`, `override_*`, or `overwrite`; do not model them.
- `AgentStudioService extends BaseRestService` over the thirteen `/api/agent-studio/*` routes. The
  API version comes from `auth.getApiDictionary(restUri)?.Version`; absent means fail loudly.
- `409` has three causes the UI must tell apart by message: stale agent revision, stale type-defaults
  revision, and a layer under a sealed one. `429`/`503` carry `Retry-After`.

## SAIL-A2 — prompt layer editor

One component per prompt section, a parent per language.

- Render `layers[]` in the order received (widest scope first); never re-sort them.
- `editable: false` layers are read-only, including the first (`scope_kind: "platform"`).
- An editable layer edits `instruction`, `output`, `merge_mode` (`append` | `replace`), and `sealed`.
  `replace` needs an instruction; under `append` an output alone is valid. Mirror both rules in the
  form, and leave the 16000-character limit to the server's field errors.
- A `sealed` layer disables every layer below it and says which scope sealed the section. Sealing the
  type layer while the agent layer has content is a save the server refuses — warn before sending.
- Adding a layer means adding one at `agent_scope_id` or `type_scope_id`, which the draft response
  names; insert the type layer before the agent layer, both after every read-only layer.
- **Save sends every layer back.** An editable layer that is missing from the request ends its
  binding, so an agent-only edit must still return the type layer unchanged.
- Show `effective_*` as the server returned it; do not merge on the client. After a save, replace the
  whole draft with the response.
- Reset offers `agent_override`, `type_default`, `platform_baseline`, optionally for one section, one
  language, or both.

## SAIL-A3 — release section provenance

`release-sections` rows carry `source {scope_id, scope_kind, merge_mode, sealed}`: the layer that
decided the section when the release was compiled. Show it beside each frozen section, with a sealed
marker. It is history; it never changes after publication.

## SAIL-A4 — typed turn events

`agent/model/turn_event.ts` for `domain.TurnEvent` (`v`, `kind`, and exactly the member its kind names):
`text_delta`, `tool_proposal`, `tool_result`, `approval_pending`, `approval_resolved`, `evidence`,
`effect`, `progress`, `result`, `extension`. Unknown kinds and unknown `extension.type` values are
ignored, not errors.

A `TurnEventList` component renders a turn's events in frame order:

- `tool_proposal`/`tool_result` pair by `call_id`; `is_error` results show the error class only.
- `effect` (new) pairs with its call by `call_id` and shows `observation.status`:
  `satisfied`, `violated`, or `unknown`, with `reason`; `reconciled: true` means the effect already
  held and nothing was sent again. `unknown` is not a failure and not a success — give it its own
  state. Evidence entries are references (`URI`, `Digest`), not content.
- `approval_pending` ends the delivery, not the turn: keep the conversation open for the resumed frames.

Transport stays the app's (`RealtimeService` channels or SSE); Sail carries and renders the frames.

## SAIL-A5 — reply stream resume and cancellation

- Frames carry `Sequence`. A client that reconnects resumes from the last sequence it rendered and
  drops a frame it already has.
- A cursor behind the retained window is an explicit "replay expired" answer. Handle it by reading the
  turn's stored result, not by showing a gap.
- A final frame may be synthesized from the turn record when the buffer no longer holds the stream: it
  has a payload or an error code and no intermediate events. Render it as a complete answer.
- Cancel is a request, not an instant stop: show "cancelling" until the final frame arrives with
  `ErrorCode` `canceled`. A suspended turn cancels the same way.

## SAIL-A6 — decision chain timeline

A read-only timeline over an app-provided audit endpoint (`contract.AuditQuery`): one request's
records from `turn_admitted` to its terminal state, grouped by category, with principal, outcome,
reason, and the authority chain when present. Each decision appears once; do not dedupe on the client.
