# Compaction — logic reference in diagrams (2026-11-12)

Purpose: a self-contained, implementation-agnostic spec of the compaction algorithm, for any
agent harness to implement. Diagrams follow the `path / zone` cell notation; every rule shown
is load-bearing (each traces to a decision the design made on evidence, not taste).
Three knobs used throughout, all percentages of the context window `n_ctx`:

```
   0%                                                    100% n_ctx
   ├───────────────┬──────────────────────┬───────────────▲──┤
   0%          budget%               threshold%          98% (in-loop hard line)
   default 20%     default 70%
   kept tail       trigger line / summary cap share
```

---

## 1. The data model

A compaction is **an ordinary entry** on the path (role `user`, content = marker line + summary).
It is not a header field, not a side table. "Effective path" is the only magic:

```
 effective path = the newest compaction entry on the path
                  + every entry strictly after its boundary entry
 (boundary = the id of the first entry that survived; if the field is absent, kept is
   deliberately empty — that is a valid state, NOT an error)

 a compaction whose boundary is missing from the path (deleted entry) is BROKEN:
   fall back to "no compaction" (the whole path raw), and report.
```

```
        ┌─────┬─────┬──────┬─────┬─────┬──────┬─────┬──────┬─────┬──────┐
  path  │ hdr │ usr │ ass │ tool │ usr │ cmp₁ │ ass │ tool │ ass │ tool │
        ├─────┼─────┼──────┼─────┼─────┼──────┼─────┼──────┼─────┼──────┤
  zone  │ head│DROP │ DROP│ DROP │ DROP│ head │ kept│ kept │ kept│ kept │
        └─────┴─────┴──────┴─────┴─────┴──────┴─────┴──────┴─────┴──────┘
                                   ↑ boundary = entry 7 (the first kept entry)
  what is sent  = [ hdr ][ cmp₁ as one user message ][ kept... verbatim ]
```

* `hdr` (the system prompt) is never in a summarize region and never dropped; it is excluded
  from all cut-point search math (a fixed, never-summarized head).
* `cmp₁` travels as a plain user message. Any entry whose kind is compaction counts as a
  **user** message in all boundary geometry (it may be a boundary; it counts toward
  "minimum kept user turns").

---

## 2. When it fires

```
 trigger line  = threshold% × n_ctx            (default 70%)
 fired by the REAL prompt tokens of the last model answer, never by an estimate;
 if no real number exists yet, automatic triggers do not fire at all.

 T1 end-of-run      usage > line after a run completes                  (the normal one)
 T2 in-loop         before sending, planned size > 98% × n_ctx          (last-minute)
 T3 manual          operator button. Bypasses "the line was not crossed"
                    and "no real number yet"; NEVER bypasses:
                    ✗ mid-generation        ✗ an unanswered tool call at the tail
                      (its wake-up would land after a fresh boundary → double answer)
 rescue            provider answered context-overflow → force one compaction
                    even if the measured numbers say otherwise.
```

---

## 3. Choosing the cut — three shapes

Constraints, checked in this order:

```
 ① legalBoundary: the first KEPT entry may be user or assistant — NEVER a tool result.
    (a tool result whose call was summarized is orphaned; if a floor would land inside a
     span, round OUTWARD to the nearest legal entry — paying tokens beats breaking protocol)
 ② kept tail      ≤ keepBudget% × n_ctx, unless a floor below lifts the allowance
 ③ summarized    ≥ minSummarizeTokens (default 2000) — tiny regions are not worth a call
 ④ floors         kept ≥ minKeptMessages; phase-1 cuts additionally keep
    ≥ minKeptUserTurns user messages. Floors only ever RAISE the allowance (kept may
    exceed budget; "the floor wins over the cap", and the log says so).
 ⑤ search         phase 1: prefer boundaries at user-message edges (whole turns kept)
                  phase 2: any legalBoundary, including mid-turn (split) and end-of-path
```

**Diagram (a) full-turn cut — the normal case**

```
 budget = 20% × 100k = 20k tokens
        ┌─────┬─────┬─────┬──────┬─────┬─────┬──────┬──────┬─────┬──────┐
  path  │ hdr │ usr │ ass │ tool │ usr │ ass │ tool │ tool │ ass │ tool │
  size  │ 1k  │ 9k  │ 3k  │ 12k  │ 4k  │ 2k  │ 8k   │ 5k   │ 1k  │ 3k   │
        ├─────┼─────┼─────┼──────┼─────┼─────┼──────┼──────┼─────┼──────┤
  zone  │ head│ DROP│ DROP│  DROP│ kept│ kept│ kept │ kept │ kept│ kept │
        └─────┴─────┴─────┴──────┴─────┴─────┴──────┴──────┴─────┴──────┘
                                 ↑                             suffix(kept)=4+2+8+5+1+3=23k ✗
                                 ↑ first user whose suffix fits → cut one turn later:
        │ usr │ ass │ tool │ tool │ ass │ tool │
        │ DROP│ DROP│ DROP │ DROP │ DROP│ DROP │     suffix=2k ✓  summarized=30k ✓

  actually sent after compaction:
      [ hdr ][ cmp(user, "earlier messages were compacted: …") ][ usr ass tool tool ass tool ]
```

**Diagram (b) split cut — one span alone does not fit the budget**

```
        ┌─────┬─────┬─────┬──────┬─────┬──────┬──────┬─────┬──────┐
  path  │ hdr │ usr │ ass │ tool │ ass │ tool │ tool │ ass │ tool │
        ├─────┼─────┼─────┼──────┼─────┼──────┼──────┼─────┼──────┤
  zone  │ head│ DROP│ DROP│  DROP│ DROP│  DROP│  DROP│ kept│ kept │
        └─────┴─────┴─────┴──────┴─────┴──────┴──────┴─────┴──────┘
                                                     ↑
   boundary before entry 8 (an assistant) → legal (assistant may start the kept region);
   isSplitTurn = true → the summarization instruction must additionally preserve
   "(b) the turn that was in progress: what was asked, what was attempted, the immediate
    next step"; the span's user message (entry 2) is inside the summarized region, so the
   summary owns the task goal.
```

**Diagram (c) empty kept — compact everything**

```
        ┌─────┬─────┬─────┬──────┬─────┬──────┬──────┬─────┬──────┐
  path  │ hdr │ usr │ ass │ tool │ ass │ tool │ tool │ ass │ tool │
        ├─────┼─────┼─────┼──────┼─────┼──────┼──────┼─────┼──────┤
  zone  │ head│ DROP│ DROP│  DROP│ DROP│  DROP│  DROP│ DROP│ DROP │
        └─────┴─────┴─────┴──────┴─────┴──────┴──────┴─────┴──────┘
                                                                  ↑ boundary field = ABSENT
   summarize region = everything; kept = empty; payload = [ hdr, cmp ].
   Any message the user sends afterwards automatically becomes the kept region.
   (absent ≠ broken: "deliberately empty" is fine, "boundary id missing from path" is
    the error state and disables the compaction entirely)
```

**Diagram (d) re-compaction — summaries are re-summarized**

```
 the active summary is always the FIRST entry of the next window, therefore always inside
 the next summarize region:

        ┌─────┬─────┬─────┬──────┬─────┬──────┬─────┬──────┐
  path  │ cmp₁│ usr │ ass │ tool │ ass │ tool │ ass │ tool │
        ├─────┼─────┼─────┼──────┼─────┼──────┼─────┼──────┤
  zone  │ DROP│ DROP│ DROP│ DROP │ DROP│ DROP │ kept│ kept │
        └─────┴─────┴─────┴──────┴─────┴──────┴─────┴──────┘
   N-th hand summaries read like legends, not transcripts — accepted deliberately:
   the prefix-caching win outweighs it, and the floors (③④) are what stop the spiral
   into summarizing summaries forever.
```

---

## 4. The summary request — shape matters as much as the prompt

```
 shape A "prefix-stable" (default):
   [ system ][ m₁ ][ m₂ ] … [ m_{k-1} ][ user: instruction ]
   └──────── byte-identical to the real conversation request through m_{k-1} ────────┘
   k = the cut. The summary call REUSES the provider's prefix cache; it pays only for the
   instruction + generation. Requirements: same model, same thinking/prompt-build flags
   (ANY of them changes the prompt bytes and splits the cache), same serialization.
   Only temperature and max_tokens may differ (post-prompt sampling; they cannot split a
   prefix). If the payload is not provably a prefix of the conversation (edit, restart,
   tree walk), fall back honestly and log "expect a full prefill".

 shape B "transcript" (escape hatches only):
   [ system: instruction ][ user: "Conversation to compress:" + flat dump ]
   Used when shape A cannot be built: summaryMode=transcript, the payload ids do not map
   to the messages, or the prefix STILL exceeds the window (pathologically oversized
   entries — clip them oversized-first: keep head+tail of each, and tell the summarizer
   verbatim how many characters were withheld; summarizing a fiction is worse than lossy).

 acceptance: summary tokens ≤ summaryCap% × n_ctx (one number doubles as this ceiling and
 as max_tokens of the call). Count the TEXT share only: completion tokens bill text +
 reasoning; prorate by bytes. Oversize after N retries → ask a human:
 [ keep it anyway | retry once more with "previous attempt was N, cap M" | skip ].
```

---

## 5. Replay — when the window is already over the line

If the context OVERFLOWED (or grew past the line while compaction slept: auto off, manual
only), a single summarize-the-whole-payload call would be huge, lossy, and prompt-ugly.
Instead: **compact repeatedly, exactly the way it would have happened one message at a
time.** No new knob; every rule above binds each round; a normal single trigger stays a
single iteration (the loop is invisible).

```
 virtual path (Tipping): walk the current window adding each entry's size; stop at the
 first entry m* that pushes the total past the trigger line. Entries after m* "do not
 exist this round" — they were not there when the normal trigger would have fired.

 round 1:  [ cmp₀ ][ …entries… ][ m* ▓over▓ ][ …hidden… ]
           virtual path = everything through m* → normal cut → summary₁ → commit cmp₁
 round 2:  window' = [ cmp₁ ][ kept₁ ] → walk → crosses at m† → cut → summary₂ → commit
   ...     until the rebuilt size estimate is back under the line (each round strictly
           shrinks the region; a max-rounds guard exists against broken sizes, not long
           chats).

 round-k request = the byte prefix of the round-(k−1) window through its cut
           (= the payload the conversation would really have been sending at that point)
           → the prefix cache carries from round to round, as it would have in the world
           where compaction never slept.
 bookkeeping: each round's entries bill exactly that round's summary calls; a session's
           compaction entries sum to the run total — no double-count, no third number.
 skip in round k stops the whole replay (same semantics as stop in the per-message
 world); rounds already committed are real compactions and stay.
```

---

## 6. Commit

```
 append ONE record: compaction entry (role user, marker + summary), boundary = first kept
 id (absent ⇔ empty kept), parent = tip, plus usage/model/attempts of this round.
 The tip advances in the same record — no observable half-committed state.
 A stop that lands after the last generation still writes nothing.
 After commit: the "real usage" number is void (it paid for the replaced payload);
 re-derive the rebuilt payload size from the estimator until the next real answer.
```

## 7. Invariants (the ones that break harnesses when violated)

```
 1. Never summarize: the system head, or anything after the boundary.
 2. A kept region never begins with a tool result; floors round outward, never inward.
 3. Real usage triggers; estimates only plan. (A trigger that guesses causes phantom
    rounds; an estimate used as a trigger costs a real model call.)
 4. The summary request shares the conversation's identity (model, thinking, prompt bytes)
    or admits in the log that it will not.
 5. Compaction never runs mid-generation, and never strands an unanswered tool call.
 6. Summary content is committed only under the cap or a human decision; silent oversized
    commits do not exist.
 7. Replacement is not deletion: history stays on the path for replay/branch/audit; only
    the effective path shrank.
```
