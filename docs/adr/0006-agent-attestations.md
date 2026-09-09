# ADR-0006: Agent attestations & mechanical approval gates

- Status: Accepted (design direction; implementation lands P2 — Open Nitro)
- Date: 2026-09-09
- Closes: #16

## Context

Pillar 15 (Open Nitro) commits us to: "Agent never writes prod without human
Approve + validation + audit" and a fully transparent cost meter (spec §21 #30).
The hard part is making those guarantees *provable* rather than promised —
trustworthy to an enterprise buyer who cannot read our source.

Two production systems solve exactly this and are open for study:

1. **awslabs/aidlc-workflows** (MIT-0, v2.8.1) — deterministic engine + hooks
   for AI workflows. Key mechanism: the plan-approval-guard *hook* blocks
   code-generation tool calls until an approved plan exists, verified by a
   **fingerprint over the exact approved bytes**. Post-approval modification
   re-blocks. Their layering rule: "determinism belongs in tools and hooks,
   knowledge in agents, judgment with humans."
2. **ai-sdlc-framework/ai-sdlc** (Apache-2.0, 48 RFCs) — governance + autonomous
   execution. Key mechanisms: **DSSE attestation envelopes** wrapping every
   change (harness identity, inputs hash, outputs, reviewer chain, signature)
   and **reviewer independence by construction** — the envelope's `harness`
   field is checked by `verify-attestation`; the implementer's harness cannot
   be any reviewer's harness.

## Decision

Open Nitro agents (P2) will implement three mechanisms, adapted to our Go
stack:

### 1. Approval fingerprints (not vibes)

Agent dry-run sheets (the preview a human approves) are content-hashed at
approval time. `execute` refuses if the sheet changed after approval.
Approval state is append-only: `approved_at`, `approver_id`,
`sheet_sha256`, stored alongside the run.

- Migration Agent: mapping + transforms sheet is fingerprinted.
- Workforce Agent: configuration sheet is fingerprinted.
- Doc Agent: generated-document outline is fingerprinted.

### 2. Signed run envelopes

`agent_runs` rows carry a DSSE-style envelope: schema-versioned JSON
(payload = prompt hash, model, input dataset hash, outputs, cost breakdown,
approvals) signed with a per-workspace key. Audit = verify signatures, not
trust logs. Envelope schema lives in `packages/contracts` beside OpenAPI.
Kill switch (per workspace, per agent) revokes the signing key.

### 3. Reviewer independence, mechanically

Agent output review (the "review" state before human gate) must run on a
different harness than the one that produced the output. The envelope
records producer harness; the reviewer step refuses same-harness pairs.
Model diversity is cheap for us (BYO-LLM router already exists in the design)
and makes "the AI checked its own homework" structurally impossible.

## Consequences

### Positive
- Open Nitro's audit story becomes a *selling point*: signed, tamper-evident,
  mechanically reviewable — visible in the agent cost dashboard (§21 #30)
- Enterprise buyers can verify agent behavior without reading source
- Self-review and post-approval drift — the two classic AI-agent failure
  modes — are eliminated at the mechanism level, not the policy level

### Negative
- Key management per workspace (rotation, revocation) — new operational
  surface; mitigate: keys in the secrets vault, rotation on kill-switch use
- Envelope schema is a public contract — versioning discipline required
  from day one (v1, additive-only)
- Reviewer independence requires a second LLM configured — small setups can
  disable agents entirely (they're opt-in already)

### Neutral
- DSSE specifically is not required; any signed-envelope scheme with the
  same properties works. We choose envelope shape in implementation.

## Alternatives considered

| Alternative | Why not |
|---|---|
| Plain audit_log rows (current spec §15 wording) | Trust-the-database; no tamper evidence, no external verifiability |
| Human review of every agent output, no machinery | Does not scale past the first customer; mechanism makes the 95% case automatic |
| Prompt-level "wait for approval" instructions | aidlc's research position: prompts are advisory, hooks are enforcement — we agree |
