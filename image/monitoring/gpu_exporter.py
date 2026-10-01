"""Minimal Prometheus exporter for the GPU (nvidia-smi), 127.0.0.1:9101/metrics."""
import subprocess
from http.server import BaseHTTPRequestHandler, HTTPServer

Q = 'utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu,clocks.sm'
NAMES = ['llm_gpu_util_percent', 'llm_gpu_mem_used_mib', 'llm_gpu_mem_total_mib', 'llm_gpu_power_watts', 'llm_gpu_temp_celsius', 'llm_gpu_sm_clock_mhz']

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            vals = subprocess.check_output(['nvidia-smi', '--query-gpu=' + Q, '--format=csv,noheader,nounits'], text=True, timeout=5).splitlines()[0].split(',')
            body = ''.join(f'# TYPE {n} gauge\n{n} {v.strip()}\n' for n, v in zip(NAMES, vals) if v.strip() not in ('', '[N/A]'))
        except Exception as e:
            body = f'# error {e}\n'
        self.send_response(200); self.send_header('Content-Type', 'text/plain; version=0.0.4'); self.end_headers(); self.wfile.write(body.encode())
    def log_message(self, *a): pass

HTTPServer(('127.0.0.1', 9101), H).serve_forever()
