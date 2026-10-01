"""TabbyAPI entry point with monitoring for the existing Grafana dashboard."""
import threading
from observability import EventLogger, CaptureMiddleware
events=EventLogger()
# known end reasons start at 0: a counter series that first appears at 1 is invisible to increase()
_endings={r:0 for r in ['stop_token','stop_string','max_new_tokens','loop_detected','cancelled','error','interrupted']}

from fastapi import Depends
from fastapi.responses import PlainTextResponse
import main
from common import gen_logging, model
from common.auth import check_api_key
from endpoints import server
_lock=threading.Lock()
_counters={n:0.0 for n in ['llm_prompt_tokens_total','llm_generated_tokens_total','llm_prefill_seconds_total','llm_decode_seconds_total','llm_requests_completed_total','llm_draft_accepted_tokens_total','llm_draft_rejected_tokens_total',
                            'llm_live_prompt_tokens_total','llm_live_generated_tokens_total','llm_live_prefill_seconds_total','llm_live_generate_seconds_total']}
# Live counters: the *_total counters above grow only when an answer ends, so rate() over them is not a speed.
# These grow while the answer is being written, from the progress events the generation loop already sends
# to the console status line (prompt position after each prefill chunk, generated token count after each chunk).
# Tokens and the seconds spent on them are counted at the same events, so tokens/seconds over any window is the
# real speed while the model works, and there is no value at all while it is idle.
import time as _time
from common import status_display as _sd
_JS=_sd.JobStatus
_js_started,_js_prefill,_js_generated=_JS.started,_JS.prefill,_JS.generated
def _live_add(name,n):
 if n>0:
  with _lock:_counters[name]+=n
def _live_tick(self,name=None):
 # seconds since this job's previous progress event; a long gap means the job was paused, not working
 now=_time.monotonic();prev=getattr(self,'_live_t',None);self._live_t=now
 if name and prev is not None and now-prev<30:_live_add(name,now-prev)
def _live_read(self,position):
 # prompt tokens really read = position reached minus the part taken from the cache; model.py sets cached_now at
 # every event because the job skips cached pages inside prefill, after the 'started' event
 read=position-getattr(self,'cached_now',getattr(self,'_live_cached',0))
 done=getattr(self,'_live_read_n',None)
 if done is None:return False
 if read>done:
  # a few leftover tokens (fully cached prompt, read together with the first answer token) say nothing about speed
  _live_tick(self,'llm_live_prefill_seconds_total' if read-done>=64 else None)
  _live_add('llm_live_prompt_tokens_total',read-done);self._live_read_n=read
 else:
  _live_tick(self)  # cache lookups/restores without new tokens are not reading time
 return True
def _started(self,cached_tokens):
 self._live_cached=cached_tokens;self._live_read_n=0;self._live_prompt_done=False
 self.__dict__.pop('cached_now',None)
 self._live_t=_time.monotonic()
 return _js_started(self,cached_tokens)
def _prefill(self,progress):
 _live_read(self,progress)
 return _js_prefill(self,progress)
def _generated(self,gen_tokens):
 if not getattr(self,'_live_prompt_done',True):  # the last prefill chunk ends with the first token
  _live_read(self,self.prompt_tokens);self._live_prompt_done=True
 else:
  _live_tick(self,'llm_live_generate_seconds_total')
 _live_add('llm_live_generated_tokens_total',gen_tokens-getattr(self,'_live_gen',0))
 self._live_gen=max(gen_tokens,getattr(self,'_live_gen',0))
 return _js_generated(self,gen_tokens)
_JS.started,_JS.prefill,_JS.generated=_started,_prefill,_generated
_original_metrics=gen_logging.log_metrics
def log_metrics(label,metrics,context_len,max_seq_len):
 reason=metrics.get('eos_reason') or 'unknown'
 events.emit('generation_end',request_id=metrics.get('request_id'),level='error' if reason=='loop_detected' else 'info',metrics={k:v for k,v in metrics.items() if k!='full_text' and not k.startswith('delta_')},context_len=context_len)
 with _lock:
  _endings[reason]=_endings.get(reason,0)+1
  _counters['llm_prompt_tokens_total']+=max((metrics.get('prompt_tokens') or 0)-(metrics.get('cached_tokens') or 0),0)
  _counters['llm_generated_tokens_total']+=metrics.get('gen_tokens') or 0
  _counters['llm_prefill_seconds_total']+=metrics.get('prompt_time') or 0
  _counters['llm_decode_seconds_total']+=metrics.get('gen_time') or 0
  if not metrics.get('partial'):
   _counters['llm_requests_completed_total']+=1
  _counters['llm_draft_accepted_tokens_total']+=metrics.get('draft_accept') or 0
  _counters['llm_draft_rejected_tokens_total']+=metrics.get('draft_reject') or 0
 return _original_metrics(label,metrics,context_len,max_seq_len)
gen_logging.log_metrics=log_metrics
# The backend imports this function directly before the API wrapper is initialized.
from backends.exllamav3 import model as exl_backend
exl_backend.log_metrics=log_metrics
_setup=server.setup_app
def setup_app(*args,**kwargs):
 app=_setup(*args,**kwargs)
 from common.errors import GenerationLoopHTTPException,generation_loop_exception_handler
 app.add_exception_handler(GenerationLoopHTTPException,generation_loop_exception_handler)
 app.add_middleware(CaptureMiddleware,events=events)
 @app.get('/metrics',dependencies=[Depends(check_api_key)],response_class=PlainTextResponse)
 async def metrics():
  with _lock:values=dict(_counters)
  values['llm_model_info']=1
  g=getattr(getattr(model.container,'generator',None),'generator',None)
  if g is not None:
   stats=g.get_cache_stats()
   if 'pending_jobs' in stats:values['llm_requests_deferred']=stats['pending_jobs']
   if hasattr(g,'active_jobs'):values['llm_requests_processing']=len(g.active_jobs)
   elif 'active_jobs' in stats:values['llm_requests_processing']=stats['active_jobs']
   if 'used_tokens' in stats:values['llm_cache_used_tokens']=stats['used_tokens']  # pages held by running requests
   # prompt cache: reusable tokens in VRAM and in the system-memory tier, capacities, and cumulative page reuse
   for k,n in [('cached_tokens','llm_cache_reusable_tokens'),('max_tokens','llm_cache_capacity_tokens'),('tier_cached_tokens','llm_cache_ram_reusable_tokens'),
               ('tier_max_tokens','llm_cache_ram_capacity_tokens'),('alloc_pages','llm_cache_pages_total'),('alloc_cached_pages','llm_cache_pages_reused_total'),
               ('alloc_tier_pages','llm_cache_pages_from_ram_total')]:
    if isinstance(stats.get(k),(int,float)):values[n]=stats[k]
   values['llm_context_tokens']=model.container.max_seq_len
  text=''.join('# TYPE '+k+(' counter\n' if k.endswith('_total') else ' gauge\n')+k+' '+str(v)+'\n' for k,v in values.items())
  text += ''.join('llm_requests_finished_total{reason="'+reason+'"} '+str(count)+'\n' for reason,count in _endings.copy().items())
  text += 'llm_log_records_dropped_total '+str(events.dropped)+'\nllm_log_write_errors_total '+str(events.errors)+'\nllm_log_body_enabled '+str(int(events.body_enabled))+'\n'
  return PlainTextResponse(text,media_type='text/plain; version=0.0.4')
 return app
server.setup_app=setup_app
if __name__=='__main__':main.entrypoint()
