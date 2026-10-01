"""Download pinned HF revisions listed in manifests/<name>.json; verify size and LFS SHA256.
Large files go through aria2c with resume (-c) so a slow/unstable link does not restart 8 GB shards from zero.
Files already present with the right size/SHA are kept."""
import json, urllib.request, hashlib, time, sys, subprocess
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor

ROOT = Path('/opt/llm')

def sha256(p):
    h = hashlib.sha256()
    with p.open('rb') as f:
        while chunk := f.read(16 * 1024 * 1024): h.update(chunk)
    return h.hexdigest()

def ok(p, f):
    if not p.exists() or (f.get('size') and p.stat().st_size != f['size']): return False
    return not f.get('sha256') or sha256(p) == f['sha256']

def fetch(args):
    manifest, f, dest = args
    p = dest / f['name']; p.parent.mkdir(parents=True, exist_ok=True)
    if ok(p, f):
        print(json.dumps({'file': str(p), 'kept': True}), flush=True); return
    url = f"https://huggingface.co/{manifest['repo']}/resolve/{manifest['revision']}/{f['name']}"
    for attempt in range(6):
        started = time.time()
        try:
            if (f.get('size') or 0) > 50_000_000:
                r = subprocess.run(['aria2c', '-x8', '-s8', '-c', '-q', '--file-allocation=none', '--max-tries=0', '--retry-wait=5',
                                    '--timeout=60', '-d', str(p.parent), '-o', p.name, url], timeout=3600)
                if r.returncode != 0: raise RuntimeError(f'aria2c exit {r.returncode}')
            else:
                with urllib.request.urlopen(url, timeout=120) as resp: p.write_bytes(resp.read())
            if not ok(p, f): p.unlink(missing_ok=True); raise ValueError('size/sha mismatch')
            print(json.dumps({'file': str(p), 'bytes': p.stat().st_size, 'sha256': f.get('sha256'), 'seconds': round(time.time() - started, 1)}), flush=True)
            return
        except Exception as e:
            print('retry', f['name'], repr(e), flush=True)
            if attempt == 5: raise
            time.sleep(5)

jobs = []
for label in sys.argv[1:]:
    m = json.loads((ROOT / 'manifests' / (label + '.json')).read_text())
    for f in m['files']:
        jobs.append((m, f, ROOT / 'models' / label))
jobs.sort(key=lambda j: -(j[1].get('size') or 0))
with ThreadPoolExecutor(max_workers=4) as ex:
    list(ex.map(fetch, jobs))
print('DOWNLOAD_DONE', flush=True)
