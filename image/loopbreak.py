"""SS_LoopBreak for TabbyAPI (ExLlamaV3 backend): stops degenerate verbatim loops inside the reasoning block.

A sampler stage that bans only the token which would extend an already-degenerate repetition:
  * the same token repeated >= max_run times in a row        -> that token is banned at this step;
  * an n-gram (2..max_period tokens) repeated back-to-back so that the periodic span is >= min_span tokens and
    >= min_reps repetitions                                   -> the token starting the next repetition is banned.
It acts only while the model is between <think> and </think> (thinking_only), so repetition the user asks for in
the final answer is untouched, and at the step where it fires it also bans EOS (ban_on_break) so the model cannot
"escape" the loop by ending the turn with an empty answer.

Configuration: environment variable TABBY_LOOPBREAK
  unset / "1" / "on"  -> enabled with the defaults below
  "0" / "off"          -> disabled (sampler stack is exactly the stock one)
  JSON object          -> enabled, keys override the defaults
Observability: EVENTS_TOTAL / REQUESTS_WITH_BREAKS (module counters, exported by serve.py /metrics).
Validated in LoopFix-20260930 (6.Result.md, 7.Tabby-smoke.md)."""
import json
import math
import os
import threading
import torch
from exllamav3.generator.sampler.custom import SS_Base, SS, SamplingState

DEFAULTS = {'max_run': 64, 'max_period': 48, 'min_span': 96, 'min_reps': 6, 'window': 800,
            'thinking_only': True, 'think_start': 248068, 'think_end': 248069, 'ban_on_break': [248044, 248046]}

EVENTS_TOTAL = 0
REQUESTS_WITH_BREAKS = 0
_lock = threading.Lock()


def _config():
    raw = os.environ.get('TABBY_LOOPBREAK', '1').strip()
    if raw.lower() in ('0', 'off', 'false', 'no'):
        return None
    cfg = dict(DEFAULTS)
    if raw.startswith('{'):
        cfg.update(json.loads(raw))
    return cfg


_CFG = _config()


def loopbreak_stage():
    """A fresh stage per request, or None when disabled."""
    return SS_LoopBreak(**_CFG) if _CFG is not None else None


class SS_LoopBreak(SS_Base):
    def __init__(self, max_run=64, max_period=48, min_span=96, min_reps=6, window=800, thinking_only=True,
                 think_start=248068, think_end=248069, ban_on_break=()):
        self.max_run = max_run; self.max_period = max_period; self.min_span = min_span; self.min_reps = min_reps
        self.window = window; self.thinking_only = thinking_only
        self.think_start = think_start; self.think_end = think_end; self.ban_on_break = list(ban_on_break)
        self.events = 0
        self._in_think = None  # last known reasoning state, used when no marker is inside the tail window

    def _need(self, n):
        return n * max(self.min_reps, math.ceil(self.min_span / n))

    def banned_for(self, tail):
        L = len(tail)
        if L < 2: return None
        last = tail[-1]; run = 1
        while run < L and tail[-1 - run] == last: run += 1
        if run >= self.max_run: return last
        for n in range(2, self.max_period + 1):
            need = self._need(n)
            if need > L: continue
            ok = True
            for k in range(1, need - n + 1):
                if tail[-k] != tail[-k - n]: ok = False; break
            if ok: return tail[-n]
        return None

    def _thinking(self, tail):
        # Latest reasoning marker inside the tail decides; otherwise keep the previous state. A request always starts
        # with the template's "<think>\n" (or "<think></think>" when reasoning is off) inside the first window.
        for t in reversed(tail):
            if t == self.think_end: self._in_think = False; break
            if t == self.think_start: self._in_think = True; break
        return bool(self._in_think)

    def run(self, state: SamplingState):
        global EVENTS_TOTAL, REQUESTS_WITH_BREAKS
        bans = []
        rows = state.past_ids
        if rows is not None:
            for b, tail in enumerate(rows[:, -self.window:].tolist()):
                if self.thinking_only and not self._thinking(tail): continue
                t = self.banned_for(tail)
                if t is not None: bans.append((b, t))
        match state.state:
            case SS.INIT:
                state.logits = state.in_logits.to(torch.float, copy=True)
                state.state = SS.LOGITS
            case SS.LOGITS:
                pass
            case _:
                raise ValueError('SS_LoopBreak must run before sorting/probability stages')
        for b, t in bans:
            state.logits[b, t] = -float('inf')
            for extra in self.ban_on_break:
                state.logits[b, extra] = -float('inf')
        if bans:
            with _lock:
                if self.events == 0: REQUESTS_WITH_BREAKS += 1
                EVENTS_TOTAL += len(bans)
            self.events += len(bans)

    def reqs_past_ids(self):
        return True
