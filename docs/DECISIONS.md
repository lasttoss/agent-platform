# Decisions taken from other projects

Each line names what was borrowed, from where, and how this repo's version differs. The point of
writing it down is that six months from now "did I think of this or did I read it" has an answer,
and an answer is what makes the difference defensible out loud.

| # | Idea | Source | What changed here |
|---|------|--------|-------------------|
| 1 | A run id that joins every step of one piece of work | Langfuse trace → span → observation | Kept as an opaque 128-bit value with no user id inside, written to the durable audit line rather than only to a sampled trace, and accepted from the caller when well formed so separate HTTP calls can be stitched into one run. Also served back as `GET /v1/run?run_id=`. |
| 2 | Interruptions: a step may wait for a person before continuing | LangGraph checkpointer / interrupt | **Not implemented.** Recorded here because it is the largest gap against the world: this gateway answers one request at a time and has no durable mid-step state. `jobs.go` says so out loud. |
| 3 | Approve or reject a tool call before it acts | OpenAI Agents SDK guardrails | Enforced at the gateway, not inside the model's instructions: a role that may not see a tool never learns it exists. |
| 4 | Guardrail layers: model-based, rule-based, moderation | OpenAI *A practical guide to building agents* | Same layering for the outbound path, but the authority layer is policy, not a prompt. Nothing here asks the model to police itself. |
| 5 | A skill is a versioned file with a stop condition, not a prompt | GitHub, *A practical guide to agent skills* (Nov 2026) | `skill-gate-lab` measures skills in three tiers; this repo runs the gate. The GitHub post says a skill "does not grant access" — the gateway is where that sentence becomes true. |
| 6 | Cache entries must die when permissions change | Not taken from anywhere; learned from a real bug | The policy generation is part of the cache key, so a role change retires stale answers without walking the cache. Nothing in the four vendor documents covers it, because they cache per API key, not per person. |

## What this file deliberately does not claim

No code was copied. The four vendor documents were read for *what problem they name*, and each row
above says how this repo answers it differently. Where the answer is "not implemented", row 2 says
so rather than implying parity.

| 7 | A test is written to prove that guarding A will expose a fault in B | Not taken from anywhere; learned twice in this repo | The cache test for instant revocation exposed `/v1/respond` never applying the console's role at all; the "a tool result is a pointer, not a body" test then exposed a read that cut a UTF-8 character in half. Both tests were written for the stated behaviour and the second fault fell out of them. The rule is recorded because it changes what a test is for: not to cover a line, but to put pressure on the line next to it. |
