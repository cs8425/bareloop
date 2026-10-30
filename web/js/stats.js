// The tokens-and-cost panel: what the conversation on screen has cost, and how much of it was a miss.
//
// Two rules own this file.
//
// (1) The population is the path the transcript is drawing. Off-path branches, failed retries and the
// request that produced a compaction summary are therefore NOT in these totals, which makes the money a
// lower bound — so the panel says that out loud instead of implying a bill it cannot see.
//
// (2) Only additive numbers are totalled. A rate is neither: a warm request re-sends a handful of
// uncached tokens while a cold one prefills thousands, so averaging their speeds invents a number that
// belongs to neither (r29, D27). There is exactly one rate per request, in the table below, and no
// average of them anywhere. Total milliseconds are additive, so the time spent prefilling and decoding
// is totalled — a sum of seconds, not a mean of speeds.
import { $, el, fmtN, pathOf, sortByNum, tps, MIN_UNCACHED } from './util.js';
import { state } from './state.js';
import { get } from './api.js';
import { note } from './ui.js';
import { openPanel, panelMode } from './panel.js';
import { EV_REFRESH, EV_SESSION, on } from './bus.js';

// fmtMoney carries no symbol on purpose: the unit is the user's own business (r29, D28), and a currency
// sign in front of a number that came from their config would be a guess about it.
const fmtMoney = (v) => (!v ? '0' : v >= 1 ? v.toFixed(2) : v.toPrecision(2));

// fmtDur is for sums of milliseconds, which is what a whole conversation adds up to.
const fmtDur = (ms) => (ms < 1000 ? `${Math.round(ms)} ms` : ms < 120000 ? `${(ms / 1000).toFixed(1)} s` : `${(ms / 60000).toFixed(1)} min`);

// costOf prices one request. `prompt` already contains `cache` (the one meaning this project settled
// on), so the uncached part — the part a provider charges full price for — has to be subtracted here.
function costOf(u, p) {
  const cached = u.cache || 0;
  const uncached = Math.max((u.prompt || 0) - cached, 0);
  const input = (uncached / 1e6) * (p.input || 0);
  const cacheCost = (cached / 1e6) * (p.cachedInput || 0);
  const out = ((u.completion || 0) / 1e6) * (p.output || 0);
  return { input, cache: cacheCost, out, total: input + cacheCost + out };
}

export function wireStats() {
  const bar = $('.ctxbar');
  if (!bar) return;
  // The context bar already answers "how full is the window"; it is the one element in the status bar
  // about tokens, so it is the door to the rest of the answer. It is a span, so it is given the role,
  // the name and the keys a button would have had.
  bar.onclick = () => showStats().catch(() => {});
  bar.onkeydown = (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      showStats().catch(() => {});
    }
  };
  // While it is open the panel is a running total, so it follows the transcript. Both handlers re-read
  // the session themselves: `refresh()` has the data but hands it to the transcript only.
  on(EV_REFRESH, () => {
    if (panelMode() === 'stats') showStats().catch(() => {});
  });
  on(EV_SESSION, () => {
    // Money belonging to another session on screen is worse than a closed panel.
    if (panelMode() === 'stats') showStats().catch(() => {});
  });
}

async function showStats() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const data = await get('').catch(() => null);
  if (!data) return;
  const st = data.status || {};
  const pricing = (data.config && data.config.llm && data.config.llm.pricing) || null;
  const entries = sortByNum(data.entries || []);
  const onPath = pathOf(entries, st.tip);
  // Only entries that carry usage are requests: user messages, tool results and compaction markers
  // have none, and counting them as rows would make every number below look like a mystery.
  const requests = entries.filter((e) => onPath.has(e.id) && e.usage && (e.usage.prompt || e.usage.completion));

  const body = openPanel('stats', `tokens and cost · ${state.id}`);

  const t = { prompt: 0, cache: 0, completion: 0, msIn: 0, msOut: 0 };
  for (const e of requests) {
    const u = e.usage;
    t.prompt += u.prompt || 0;
    t.cache += u.cache || 0;
    t.completion += u.completion || 0;
    t.msIn += u.promptMs || 0;
    t.msOut += u.completionMs || 0;
  }
  const uncached = Math.max(t.prompt - t.cache, 0);

  const sum = el('div', 'tsum');
  sum.appendChild(
    el(
      'div',
      '',
      `${requests.length} request${requests.length === 1 ? '' : 's'} on this path · ${onPath.size - requests.length} entries without usage (your messages, tool results)`,
    ),
  );
  // Not the `.num` class: that right-aligns, which is what a column of figures wants and not what a
  // sentence of totals wants.
  const line = el('div');
  // `var(--t-2)`, not `13px`: an inline px size is the one thing app.css's scale cannot reach, and this is
  // the headline sentence of the panel - with a literal it stayed 13px at every text size the user picks.
  line.style.fontSize = 'var(--t-2)';
  line.textContent = `${fmtN(t.prompt)} prompt · ${fmtN(t.cache)} cached (${t.prompt ? `${Math.round((100 * t.cache) / t.prompt)}%` : '—'}) · ${fmtN(uncached)} new · ${fmtN(t.completion)} out`;
  line.title =
    'prompt tokens include the cached ones — that is how the provider reports them, and adding the two would count them twice.\n' +
    'new = prompt − cached: the tokens a miss made this server compute again.\n' +
    'hit rate = Σ cached / Σ prompt.';
  sum.appendChild(line);
  if (t.msIn || t.msOut) {
    const time = el('div', 'cost');
    time.textContent = `time in the model: ${fmtDur(t.msIn)} prefill · ${fmtDur(t.msOut)} decode${t.msIn && t.msOut ? ` · ${fmtDur(t.msIn + t.msOut)} total` : ''}`;
    time.title =
      'sums of the per-request milliseconds the server reported (llama.cpp timings), so they only cover requests that reported them.\n' +
      'No tok/s is derived from these: total time over total tokens mixes a cold prefill with a warm request, and that number describes neither.';
    sum.appendChild(time);
  }
  body.appendChild(sum);

  // Money: three components and a total (r29, D28). With no price set the panel does not talk about
  // money at all — it says what would make it start.
  if (pricing) {
    const c = costOf({ prompt: t.prompt, cache: t.cache, completion: t.completion }, pricing);
    const money = el('table');
    money.style.marginTop = '8px';
    const mrow = (what, tokens, value, last) => {
      const tr = el('tr', last ? 'tot' : '');
      tr.appendChild(el('td', '', what));
      tr.appendChild(el('td', 'num', tokens));
      tr.appendChild(el('td', 'num', value));
      if (last) tr.style.borderTop = '1px solid var(--border)';
      money.appendChild(tr);
    };
    mrow('uncached input', fmtN(uncached), fmtMoney(c.input));
    mrow('cached input', fmtN(t.cache), fmtMoney(c.cache));
    mrow('output', fmtN(t.completion), fmtMoney(c.out));
    mrow('total', '', fmtMoney(c.total), true);
    body.appendChild(money);
    body.appendChild(
      el(
        'div',
        'cost',
        `per 1 000 000 tokens: input ${pricing.input ?? 0} · cached input ${pricing.cachedInput ?? 0} · output ${pricing.output ?? 0}. No currency: these are your numbers, and the unit is whatever you typed.`,
      ),
    );
  } else {
    body.appendChild(
      el(
        'div',
        'cost',
        'nothing is billed here yet. Settings has three prices (uncached input, cached input, output, per 1 000 000 tokens) — set them and this panel totals them for this conversation included, because the usage of every request is already in its log entry.',
      ),
    );
  }

  // One row per request: the only place a rate is allowed to appear, since a rate belongs to one
  // request and to nothing else (D27).
  if (requests.length) {
    const table = el('table');
    table.style.marginTop = '9px';
    const head = el('tr');
    for (const h of ['entry', 'prompt', 'cached', 'out', 'tok/s', 'cost']) head.appendChild(el('th', '', h));
    table.appendChild(head);
    for (const e of requests.slice().reverse()) {
      const u = e.usage;
      const cached = u.cache || 0;
      const newTokens = Math.max((u.prompt || 0) - cached, 0);
      // Prefill is the *new* tokens over the time the server spent on them — that is what its own
      // prompt_per_second divides, and the only version that does not count cached tokens as work.
      const inTps = tps(newTokens, u.promptMs, MIN_UNCACHED);
      const outTps = tps(u.completion || 0, u.completionMs, 4);
      const rate = `${inTps ?? '—'} / ${outTps ?? '—'}`;
      const tr = el('tr');
      const id = el('td', '', e.id);
      const bits = [];
      if (u.prompt) bits.push(`prompt ${u.prompt}${cached ? ` (cached ${cached}, new ${newTokens})` : ''}`);
      if (u.completion) bits.push(`out ${u.completion}`);
      if (u.promptMs) bits.push(`prefill ${Math.round(u.promptMs)} ms`);
      if (u.completionMs) bits.push(`decode ${Math.round(u.completionMs)} ms`);
      if (u.draftN) bits.push(`speculative decode ${u.draftAccepted}/${u.draftN} accepted`);
      if (e.ms) bits.push(`${e.ms} ms end to end`);
      if (e.finishReason) bits.push(e.finishReason);
      id.title = bits.join('\n');
      tr.appendChild(id);
      tr.appendChild(el('td', 'num', fmtN(u.prompt || 0)));
      tr.appendChild(el('td', 'num', cached ? fmtN(cached) : '—'));
      tr.appendChild(el('td', 'num', fmtN(u.completion || 0)));
      const rc = el('td', 'num', rate);
      rc.title =
        `prefill is the ${newTokens} new tokens over the server's own prompt time; decode is ${u.completion || 0} tokens over its predicted time.` +
        (inTps === null
          ? `\n${newTokens} new tokens is under ${MIN_UNCACHED}: dividing those by the fixed cost of a request is a measurement of nothing, so no prefill number is shown.`
          : '') +
        (outTps === null ? '\nToo few tokens generated (or no reported time) for a decode rate to mean anything.' : '');
      tr.appendChild(rc);
      tr.appendChild(el('td', 'num', pricing ? fmtMoney(costOf(u, pricing).total) : '—'));
      // An entry from before the server reported timings still counts towards the tokens; it simply
      // has no rate, and dimming the row says so without a legend.
      if (!u.promptMs && !u.completionMs) tr.style.opacity = 0.5;
      table.appendChild(tr);
    }
    body.appendChild(table);
    body.appendChild(el('div', 'cost', 'most recent first · tok/s is prefill / decode for that one request · dim rows are requests the server gave no timings for'));
  }

  // The lower bound, stated (r29, D29). Anything that left the path, failed before producing usage, or
  // was spent by compaction itself is real work that this view cannot see.
  body.appendChild(
    el(
      'div',
      'dim',
      `These are the ${requests.length} requests on the path the transcript is drawing — the same set the context bar prices. Not here: entries on other branches, attempts that failed before the model answered, and the request that wrote a compaction summary (its own usage stays out of the log). So the money above is a lower bound, never an upper one.`,
    ),
  );
  if (st.nCtx) {
    const hint = el('div', 'dim');
    hint.textContent = `the window itself: ${fmtN(st.contextUsed || 0)} of ${fmtN(st.nCtx)} tokens used (${st.nCtxSource || 'unknown source'} n_ctx, ${st.thresholdPct ?? '-'}% compaction trigger)`;
    body.appendChild(hint);
  }
}
