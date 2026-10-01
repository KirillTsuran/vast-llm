"""Generates dashboards/llm.json: one simple, readable page for the LLM server (run once when editing)."""
import json
from pathlib import Path

DS = {'type': 'prometheus', 'uid': 'prom'}
panels = []
y = 0

def stat(title, expr, x, w, unit='short', decimals=0, color='blue', desc=''):
    panels.append({'type': 'stat', 'title': title, 'description': desc, 'datasource': DS, 'gridPos': {'x': x, 'y': y, 'w': w, 'h': 4},
                   'targets': [{'expr': expr, 'refId': 'A', 'instant': True}],
                   'fieldConfig': {'defaults': {'unit': unit, 'decimals': decimals, 'color': {'mode': 'fixed', 'fixedColor': color}}, 'overrides': []},
                   'options': {'reduceOptions': {'calcs': ['lastNotNull']}, 'colorMode': 'value', 'graphMode': 'none', 'textMode': 'value'}})

def ts(title, targets, x, w, unit='short', h=8, desc='', stack=False):
    panels.append({'type': 'timeseries', 'title': title, 'description': desc, 'datasource': DS, 'gridPos': {'x': x, 'y': y, 'w': w, 'h': h},
                   'targets': [{'expr': e, 'legendFormat': l, 'refId': chr(65 + i)} for i, (e, l) in enumerate(targets)],
                   'fieldConfig': {'defaults': {'unit': unit, 'custom': {'lineWidth': 2, 'fillOpacity': 10, 'stacking': {'mode': 'normal' if stack else 'none'}}}, 'overrides': []},
                   'options': {'legend': {'displayMode': 'list', 'placement': 'bottom'}, 'tooltip': {'mode': 'multi'}}})

def row(title):
    global y
    panels.append({'type': 'row', 'title': title, 'collapsed': False, 'gridPos': {'x': 0, 'y': y, 'w': 24, 'h': 1}, 'panels': []})
    y += 1

row('Сейчас')
stat('Скорость генерации, ток/с', 'sum(rate(llm_generated_tokens_total[2m])) / clamp_min(sum(rate(llm_decode_seconds_total[2m])), 0.001)', 0, 4, decimals=0, color='green',
     desc='Средняя скорость вывода за 2 минуты (во время генерации)')
stat('Запросов за час', 'sum(increase(llm_requests_completed_total[1h]))', 4, 4)
stat('Сейчас генерирует', 'sum(llm_requests_processing) or vector(0)', 8, 4, color='purple')
stat('Ошибки зацикливания за сутки', 'sum(increase(llm_requests_finished_total{reason="loop_detected"}[24h])) or vector(0)', 12, 4, color='red',
     desc='generation_loop_detected — запрос оборван детектором повторов')
stat('LoopBreak вмешался за сутки', 'sum(increase(llm_loopbreak_events_total[24h])) or vector(0)', 16, 4, color='orange',
     desc='Сколько раз защита остановила вырожденный повтор в рассуждении (запрос при этом продолжается)')
stat('Загрузка GPU', 'llm_gpu_util_percent', 20, 4, unit='percent', color='yellow')
y += 4
row('Генерация')
ts('Токены в секунду', [('sum(rate(llm_generated_tokens_total[1m]))', 'генерация (среднее за минуту)'),
                        ('sum(rate(llm_prompt_tokens_total[1m]))', 'обработка входа (новые токены)')], 0, 12)
ts('Как заканчиваются запросы', [('sum by (reason) (increase(llm_requests_finished_total[5m]))', '{{reason}}')], 12, 12, stack=True,
   desc='stop_token — нормальный конец; max_new_tokens — упёрся в лимит; loop_detected — оборван из-за повтора')
y += 8
ts('Контекст в кэше, токенов', [('llm_cache_used_tokens', 'занято'), ('llm_context_tokens', 'максимум')], 0, 12)
ts('Принятие черновых токенов DFlash2, %', [('100 * sum(rate(llm_draft_accepted_tokens_total[2m])) / clamp_min(sum(rate(llm_draft_accepted_tokens_total[2m])) + sum(rate(llm_draft_rejected_tokens_total[2m])), 0.001)', 'принято')],
   12, 12, unit='percent', desc='Чем выше, тем сильнее ускорение от DFlash2')
y += 8
row('Видеокарта')
ts('Видеопамять, МиБ', [('llm_gpu_mem_used_mib', 'занято'), ('llm_gpu_mem_total_mib', 'всего')], 0, 8, unit='decmbytes')
ts('Загрузка GPU, %', [('llm_gpu_util_percent', 'загрузка')], 8, 8, unit='percent')
ts('Мощность и температура', [('llm_gpu_power_watts', 'Вт'), ('llm_gpu_temp_celsius', '°C')], 16, 8)

dash = {'uid': 'llm', 'title': 'LLM-сервер', 'timezone': 'browser', 'refresh': '10s', 'time': {'from': 'now-1h', 'to': 'now'},
        'schemaVersion': 39, 'version': 1, 'editable': True, 'panels': panels, 'templating': {'list': []}, 'annotations': {'list': []}}
Path(__file__).with_name('dashboards').joinpath('llm.json').write_text(json.dumps(dash, ensure_ascii=False, indent=1), encoding='utf-8')
print(len(panels), 'panels')
