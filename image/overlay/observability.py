"""Bounded asynchronous JSONL capture; never records HTTP auth headers."""
import json,os,queue,threading,time,uuid,shutil
from pathlib import Path
from datetime import datetime,timezone

class EventLogger:
    def __init__(self,path=None,start=True):
        self.path=Path(path or os.getenv('LLM_LOG_DIR','/app/dialog-logs'))
        self.path.mkdir(parents=True,exist_ok=True)
        self.q=queue.Queue(maxsize=2048)
        self.dropped=0;self.errors=0;self.body_enabled=True
        self.secret_values=[]
        try:
            d=json.loads(Path('/app/api_tokens.yml').read_text())
            self.secret_values=[v for v in d.values() if isinstance(v,str) and len(v)>8]
        except (OSError,ValueError):pass
        if start:threading.Thread(target=self.writer,daemon=True).start()
    def emit(self,event,**kw):
        if event in ('request_body','response_body') and not self.body_enabled:return
        r={'timestamp':datetime.now(timezone.utc).isoformat(),'level':'info','event':event,'model':os.getenv('LLM_MODEL_LABEL','qwen3.8-27b-uncensored'),'quantization':os.getenv('LLM_QUANT_LABEL','EXL3-4.0+DFlash2-4.0'),**kw}
        try:self.q.put_nowait(r)
        except queue.Full:self.dropped+=1
    def parts(self,event,request_id,text,part_start=0):
        # Redact before splitting so a key cannot escape across part boundaries.
        for value in self.secret_values:text=text.replace(value,'[REDACTED]')
        for i in range(0,len(text),8192):
            self.emit(event,request_id=request_id,part_index=part_start+i//8192,payload=text[i:i+8192])
        return part_start+(len(text)+8191)//8192
    def writer(self):
        last_housekeeping=0;last_drop=0;last_errors=0
        while True:
            now=time.time()
            if now-last_housekeeping>60:
                try:
                    enabled=shutil.disk_usage(self.path).free>5*1024**3
                    if enabled!=self.body_enabled:
                        self.body_enabled=enabled;self.emit('body_capture_state',enabled=enabled,level='warning')
                    for p in self.path.glob('dialog-*.jsonl'):
                        try:age=now-int(p.stem.split('-')[1])*600
                        except ValueError:continue
                        if age>=48*3600:p.unlink()
                except OSError:self.errors+=1
                last_housekeeping=now
            try:r=self.q.get(timeout=1)
            except queue.Empty:continue
            try:
                p=self.path/('dialog-'+str(int(now//600))+'.jsonl')
                with p.open('a',encoding='utf-8') as f:
                    if self.dropped!=last_drop:
                        f.write(json.dumps({'timestamp':datetime.now(timezone.utc).isoformat(),'level':'error','event':'log_records_dropped','count':self.dropped})+'\n');last_drop=self.dropped
                    if self.errors!=last_errors:
                        f.write(json.dumps({'timestamp':datetime.now(timezone.utc).isoformat(),'level':'error','event':'log_write_errors','count':self.errors})+'\n');last_errors=self.errors
                    encoded=json.dumps(r,ensure_ascii=False)
                    for secret in self.secret_values:
                        # Also cover diagnostic fields and JSON-escaped key values.
                        encoded=encoded.replace(json.dumps(secret,ensure_ascii=False)[1:-1],'[REDACTED]')
                    f.write(encoded+'\n')
            except OSError:self.errors+=1
            finally:self.q.task_done()

class CaptureMiddleware:
    def __init__(self,app,events):self.app=app;self.events=events
    async def __call__(self,scope,receive,send):
        if (self.events.path/'.capture-disabled').exists():
            return await self.app(scope,receive,send)
        if scope['type']!='http' or scope.get('path') not in ('/v1/chat/completions','/v1/completions'):
            return await self.app(scope,receive,send)
        rid=str(uuid.uuid4());body=bytearray();pending='';part=0;logged=False;status=0;started=time.monotonic()
        import codecs
        decoder=codecs.getincrementaldecoder('utf-8')('replace')
        def current_id():return str(scope.get('state',{}).get('id') or rid)
        def log_request():
            nonlocal logged
            if not logged:
                self.events.parts('request_body',current_id(),body.decode('utf-8',errors='replace'));logged=True
        async def recv():
            msg=await receive()
            if msg['type']=='http.request':body.extend(msg.get('body',b''))
            if msg['type']=='http.disconnect':self.events.emit('client_disconnect',request_id=current_id())
            return msg
        async def out(msg):
            nonlocal pending,part,status
            if msg['type']=='http.response.start':status=msg['status'];log_request()
            if msg['type']=='http.response.body':
                pending+=decoder.decode(msg.get('body',b''),final=not msg.get('more_body',False))
                if len(pending)>=16384 or not msg.get('more_body',False):
                    # Leave a tail longer than any known key for cross-chunk redaction.
                    n=len(pending) if not msg.get('more_body',False) else max(0,len(pending)-1024)
                    for secret in self.events.secret_values:pending=pending.replace(secret,'[REDACTED]')
                    part=self.events.parts('response_body',current_id(),pending[:n],part);pending=pending[n:]
            await send(msg)
        try:await self.app(scope,recv,out)
        finally:
            log_request()
            if pending:self.events.parts('response_body',current_id(),pending,part)
            self.events.emit('request_end',request_id=current_id(),status=status,seconds=round(time.monotonic()-started,3))
