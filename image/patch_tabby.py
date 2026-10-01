"""Integrate SS_LoopBreak into a TabbyAPI checkout. Usage: patch_tabby.py <tabby_dir>
Idempotent. Changes:
  backends/exllamav3/loopbreak.py   new file (the stage + env config + counters)
  backends/exllamav3/sampler.py     build(): prepend the stage (also kept in the greedy path)
  serve.py (production entry point)  /metrics: llm_loopbreak_events_total, llm_loopbreak_requests_total"""
import shutil, sys, hashlib, json
from pathlib import Path

T = Path(sys.argv[1]); here = Path(__file__).resolve().parent
report = {}

shutil.copy(here / 'loopbreak.py', T / 'backends/exllamav3/loopbreak.py')

p = T / 'backends/exllamav3/sampler.py'; s = p.read_text()
if 'loopbreak_stage' not in s:
    old_imp = 'from common.sampling import BaseSamplerRequest\n'
    assert s.count(old_imp) == 1
    s = s.replace(old_imp, old_imp + 'from backends.exllamav3.loopbreak import loopbreak_stage\n')
    old_build = '''        """Builds the final sampler from stack."""
'''
    new_build = '''        """Builds the final sampler from stack."""

        # Loop breaker reads raw logits, so it must be the first step; also kept under greedy decoding
        loopbreak = loopbreak_stage()
        if loopbreak is not None:
            self.stack.insert(0, loopbreak)
'''
    assert s.count(old_build) == 1
    s = s.replace(old_build, new_build)
    old_greedy = 'kept = [s for s in self.stack if isinstance(s, _GREEDY_KEPT_STEPS)]'
    assert s.count(old_greedy) == 1
    s = s.replace(old_greedy, 'kept = [s for s in self.stack if isinstance(s, _GREEDY_KEPT_STEPS) or s is loopbreak]')
    p.write_text(s)
report['sampler.py'] = hashlib.sha256(p.read_bytes()).hexdigest()

p = T / 'serve.py'
if p.exists():
    s = p.read_text()
    if 'llm_loopbreak_events_total' not in s:
        old = "  values['llm_model_info']=1\n"
        assert s.count(old) == 1
        s = s.replace(old, old + "  from backends.exllamav3 import loopbreak as _lb\n"
                                 "  values['llm_loopbreak_events_total']=_lb.EVENTS_TOTAL\n"
                                 "  values['llm_loopbreak_requests_total']=_lb.REQUESTS_WITH_BREAKS\n")
        p.write_text(s)
    report['serve.py'] = hashlib.sha256(p.read_bytes()).hexdigest()
print(json.dumps(report))
