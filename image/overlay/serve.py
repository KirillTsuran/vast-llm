"""TabbyAPI entry point with monitoring for the existing Grafana dashboard."""
import threading
from observability import EventLogger, CaptureMiddleware
events=EventLogger()
_endings={}

from fastapi import Depends
from fastapi.responses import PlainTextResponse
import main
from common import gen_logging, model
from common.auth import check_api_key
from endpoints import server
_lock=threading.Lock()
_counters={n:0.0 for n in ['llm_prompt_tokens_total','llm_generated_tokens_total','llm_prefill_seconds_total','llm_decode_seconds_total','llm_requests_completed_total','llm_draft_accepted_tokens_total','llm_draft_rejected_tokens_total']}
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
   if 'used_tokens' in stats:values['llm_cache_used_tokens']=stats['used_tokens']
   values['llm_context_tokens']=model.container.max_seq_len
  text=''.join('# TYPE '+k+(' counter\n' if k.endswith('_total') else ' gauge\n')+k+' '+str(v)+'\n' for k,v in values.items())
  text += ''.join('llm_requests_finished_total{reason="'+reason+'"} '+str(count)+'\n' for reason,count in _endings.copy().items())
  text += 'llm_log_records_dropped_total '+str(events.dropped)+'\nllm_log_write_errors_total '+str(events.errors)+'\nllm_log_body_enabled '+str(int(events.body_enabled))+'\n'
  return PlainTextResponse(text,media_type='text/plain; version=0.0.4')
 return app
server.setup_app=setup_app
if __name__=='__main__':main.entrypoint()
