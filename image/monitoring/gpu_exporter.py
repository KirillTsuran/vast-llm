"""Minimal Prometheus exporter, 127.0.0.1:9101/metrics: the GPU (nvidia-smi) and this container's own limits and usage (cgroup v2).
node_exporter sees the whole Vast host (all its RAM and CPUs); the container is capped lower, so RAM/CPU/disk I/O come from here."""
import subprocess
from http.server import BaseHTTPRequestHandler, HTTPServer

Q = 'utilization.gpu,memory.used,memory.total,power.draw,power.limit,temperature.gpu,clocks.sm,clocks.max.sm'
NAMES = ['llm_gpu_util_percent', 'llm_gpu_mem_used_mib', 'llm_gpu_mem_total_mib', 'llm_gpu_power_watts', 'llm_gpu_power_limit_watts',
         'llm_gpu_temp_celsius', 'llm_gpu_sm_clock_mhz', 'llm_gpu_sm_clock_max_mhz']
CG = '/sys/fs/cgroup/'


def gpu():
    vals = subprocess.check_output(['nvidia-smi', '--query-gpu=' + Q, '--format=csv,noheader,nounits'], text=True, timeout=5).splitlines()[0].split(',')
    return [(f'{n} gauge', v.strip()) for n, v in zip(NAMES, vals) if v.strip() not in ('', '[N/A]')]


def read(name):
    with open(CG + name) as f:
        return f.read()


def kv(name):
    return {k: float(v) for k, v in (line.split()[:2] for line in read(name).splitlines() if len(line.split()) >= 2 and line.split()[1].replace('.', '').isdigit())}


def container():
    out = []
    mem_max = read('memory.max').strip()
    stat = kv('memory.stat')
    out.append(('llm_container_memory_used_bytes gauge', read('memory.current').strip()))
    if mem_max != 'max':
        out.append(('llm_container_memory_limit_bytes gauge', mem_max))
    # what cannot be dropped under pressure (programs, shared memory) vs file cache the kernel frees when needed
    out.append(('llm_container_memory_programs_bytes gauge', stat.get('anon', 0) + stat.get('shmem', 0) + stat.get('kernel', 0)))
    out.append(('llm_container_memory_file_cache_bytes gauge', max(stat.get('file', 0) - stat.get('shmem', 0), 0)))
    out.append(('llm_container_oom_kills_total counter', kv('memory.events').get('oom_kill', 0)))
    out.append(('llm_container_cpu_seconds_total counter', kv('cpu.stat').get('usage_usec', 0) / 1e6))
    quota, period = read('cpu.max').split()
    if quota != 'max':
        out.append(('llm_container_cpu_limit_cores gauge', int(quota) / int(period)))
    rd = wr = 0
    for line in read('io.stat').splitlines():
        f = dict(p.split('=') for p in line.split()[1:] if '=' in p)
        rd += int(f.get('rbytes', 0)); wr += int(f.get('wbytes', 0))
    out.append(('llm_container_disk_read_bytes_total counter', rd))
    out.append(('llm_container_disk_written_bytes_total counter', wr))
    return out


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = ''
        for part in (gpu, container):
            try:
                body += ''.join(f'# TYPE {n}\n{n.split()[0]} {v}\n' for n, v in part())
            except Exception as e:
                body += f'# {part.__name__} error: {e}\n'
        self.send_response(200); self.send_header('Content-Type', 'text/plain; version=0.0.4'); self.end_headers(); self.wfile.write(body.encode())

    def log_message(self, *a): pass


HTTPServer(('127.0.0.1', 9101), H).serve_forever()
