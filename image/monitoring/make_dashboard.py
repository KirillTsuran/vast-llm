"""Generates the two Grafana dashboards (run after editing): dashboards/llm-metrics.json and dashboards/llm-logs.json."""
import json
from pathlib import Path

PROM = {'type': 'prometheus', 'uid': 'prom'}
LOKI = {'type': 'loki', 'uid': 'loki'}
OUT = Path(__file__).with_name('dashboards')


class Dash:
    def __init__(self):
        self.panels, self.y = [], 0

    def row(self, title):
        self.panels.append({'type': 'row', 'title': title, 'collapsed': False, 'gridPos': {'x': 0, 'y': self.y, 'w': 24, 'h': 1}, 'panels': []})
        self.y += 1

    def stat(self, title, expr, x, w, unit='short', color='blue', desc='', ds=PROM, mappings=None):
        self.panels.append({'type': 'stat', 'title': title, 'description': desc, 'datasource': ds, 'gridPos': {'x': x, 'y': self.y, 'w': w, 'h': 4},
                            'targets': [{'expr': expr, 'refId': 'A', 'instant': True, 'datasource': ds}],
                            'fieldConfig': {'defaults': {'unit': unit, 'decimals': 0, 'color': {'mode': 'fixed', 'fixedColor': color},
                                                         'mappings': mappings or []}, 'overrides': []},
                            'options': {'reduceOptions': {'calcs': ['lastNotNull']}, 'colorMode': 'value', 'graphMode': 'none', 'textMode': 'value'}})

    def ts(self, title, targets, x, w, unit='short', h=8, desc='', stack=False, ds=PROM, bars=False):
        self.panels.append({'type': 'timeseries', 'title': title, 'description': desc, 'datasource': ds, 'gridPos': {'x': x, 'y': self.y, 'w': w, 'h': h},
                            'targets': [{'expr': e, 'legendFormat': l, 'refId': chr(65 + i), 'datasource': ds} for i, (e, l) in enumerate(targets)],
                            'fieldConfig': {'defaults': {'unit': unit, 'custom': {'lineWidth': 2, 'fillOpacity': 60 if bars else 10,
                                                                                  'drawStyle': 'bars' if bars else 'line',
                                                                                  'stacking': {'mode': 'normal' if stack else 'none'}}}, 'overrides': []},
                            'options': {'legend': {'displayMode': 'list', 'placement': 'bottom'}, 'tooltip': {'mode': 'multi'}}})

    def logs(self, title, expr, x, w, h=12, desc=''):
        self.panels.append({'type': 'logs', 'title': title, 'description': desc, 'datasource': LOKI, 'gridPos': {'x': x, 'y': self.y, 'w': w, 'h': h},
                            'targets': [{'expr': expr, 'refId': 'A', 'datasource': LOKI}],
                            'options': {'showTime': True, 'wrapLogMessage': True, 'sortOrder': 'Descending', 'enableLogDetails': True, 'dedupStrategy': 'none'}})

    def save(self, uid, title, name, templating=None, links=None):
        d = {'uid': uid, 'title': title, 'timezone': 'browser', 'refresh': '10s', 'time': {'from': 'now-1h', 'to': 'now'}, 'schemaVersion': 39,
             'version': 1, 'editable': True, 'panels': self.panels, 'templating': {'list': templating or []}, 'annotations': {'list': []},
             'links': links or []}
        (OUT / name).write_text(json.dumps(d, ensure_ascii=False, indent=1), encoding='utf-8')
        print(name, len(self.panels), 'panels')


UPDOWN = [{'type': 'value', 'options': {'0': {'text': 'не работает', 'color': 'red'}, '1': {'text': 'работает', 'color': 'green'}}}]
link = lambda uid, title: [{'title': title, 'type': 'link', 'url': f'/d/{uid}', 'icon': 'dashboard'}]

# ---------------- metrics ----------------
m = Dash()
m.row('Сейчас')
m.stat('Ток/с', 'sum(increase(llm_generated_tokens_total[15m])) / clamp_min(sum(increase(llm_decode_seconds_total[15m])), 0.001)', 0, 4, color='green',
       desc='Скорость генерации модели: среднее за последние 15 минут (только время генерации)')
m.stat('Запросов/час', 'sum(increase(llm_requests_completed_total[1h]))', 4, 4)
m.stat('В работе', 'sum(llm_requests_processing) or vector(0)', 8, 4, color='purple')
m.stat('Зацикливания 24ч', 'sum(increase(llm_requests_finished_total{reason="loop_detected"}[24h])) or vector(0)', 12, 4, color='red',
       desc='generation_loop_detected: запрос оборван детектором повторов')
m.stat('LoopBreak 24ч', 'sum(increase(llm_loopbreak_events_total[24h])) or vector(0)', 16, 4, color='orange',
       desc='Сколько раз защита остановила вырожденный повтор в рассуждении (запрос при этом продолжается)')
m.stat('GPU', 'llm_gpu_util_percent', 20, 4, unit='percent', color='yellow')
m.y += 4
m.row('Генерация')
m.ts('Токены в секунду', [('sum(rate(llm_generated_tokens_total[1m]))', 'генерация'), ('sum(rate(llm_prompt_tokens_total[1m]))', 'обработка входа (новые токены)')], 0, 12)
m.ts('Как заканчиваются запросы', [('sum by (reason) (increase(llm_requests_finished_total[5m]))', '{{reason}}')], 12, 12, stack=True, bars=True,
     desc='stop_token — нормальный конец; max_new_tokens — упёрся в лимит; loop_detected — оборван из-за повтора')
m.y += 8
m.ts('Контекст в кэше, токенов', [('llm_cache_used_tokens', 'занято'), ('llm_context_tokens', 'максимум')], 0, 12)
m.ts('Принятие черновых токенов DFlash2, %', [('100 * sum(rate(llm_draft_accepted_tokens_total[2m])) / clamp_min(sum(rate(llm_draft_accepted_tokens_total[2m])) + sum(rate(llm_draft_rejected_tokens_total[2m])), 0.001)', 'принято')],
     12, 12, unit='percent', desc='Чем выше, тем сильнее ускорение от DFlash2')
m.y += 8
m.row('Видеокарта')
m.ts('Видеопамять, МиБ', [('llm_gpu_mem_used_mib', 'занято'), ('llm_gpu_mem_total_mib', 'всего')], 0, 8, unit='decmbytes')
m.ts('Загрузка GPU, %', [('llm_gpu_util_percent', 'загрузка')], 8, 8, unit='percent')
m.ts('Мощность (Вт) и температура (°C)', [('llm_gpu_power_watts', 'Вт'), ('llm_gpu_temp_celsius', '°C')], 16, 8)
m.y += 8
m.row('Машина (node_exporter)')
m.ts('CPU, %', [('100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[1m])))', 'занято')], 0, 6, unit='percent')
m.ts('Оперативная память', [('node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes', 'занято'), ('node_memory_MemTotal_bytes', 'всего')], 6, 6, unit='bytes')
m.ts('Диск /, занято', [('node_filesystem_size_bytes{mountpoint="/"} - node_filesystem_avail_bytes{mountpoint="/"}', 'занято'),
                       ('node_filesystem_size_bytes{mountpoint="/"}', 'всего')], 12, 6, unit='bytes')
m.ts('Сеть', [('sum(rate(node_network_receive_bytes_total{device!="lo"}[1m]))', 'приём'), ('sum(rate(node_network_transmit_bytes_total{device!="lo"}[1m]))', 'отдача')],
     18, 6, unit='Bps')
m.y += 8
m.row('Сервисы')
SERVICES = [('tabby', 'TabbyAPI (модель)'), ('gpu', 'Экспортёр GPU'), ('node', 'node_exporter'), ('prometheus', 'Prometheus'),
            ('loki', 'Loki'), ('alloy', 'Alloy'), ('grafana', 'Grafana')]
for i, (job, title) in enumerate(SERVICES):
    m.stat(title, f'max(up{{job="{job}"}}) or vector(0)', i * 3 + (i >= 4) * 1, 3 + (i == 3), mappings=UPDOWN)
m.y += 4
m.save('llm-metrics', 'LLM — метрики', 'llm-metrics.json', links=link('llm-logs', 'LLM — логи'))

# ---------------- logs ----------------
lg = Dash()
VARS = [{'type': 'query', 'name': 'service', 'label': 'Сервис', 'datasource': LOKI, 'query': {'label': 'service', 'type': 1, 'stream': '', 'refId': 'svc'},
         'definition': 'label_values(service)', 'includeAll': True, 'multi': True, 'current': {'text': 'All', 'value': '$__all'}, 'refresh': 2},
        {'type': 'textbox', 'name': 'search', 'label': 'Найти текст', 'query': '', 'current': {'text': '', 'value': ''}}]
SEL = '{service=~"$service"}'
ERR = '(?i)(error|exception|traceback|failed|oom|loop_detected)'
lg.row('Обзор')
lg.stat('Строк за 15 мин', f'sum(count_over_time({SEL} [15m]))', 0, 6, ds=LOKI)
lg.stat('Ошибок за час', f'sum(count_over_time({SEL} |~ "{ERR}" [1h])) or vector(0)', 6, 6, color='red', ds=LOKI,
        desc='Строки со словами error, exception, traceback, failed, oom, loop_detected')
lg.stat('Зацикливаний за сутки', 'sum(count_over_time({job="requests"} |= "loop_detected" [24h])) or vector(0)', 12, 6, color='orange', ds=LOKI)
lg.stat('Запросов/час', 'sum(count_over_time({job="requests", event="generation_end"} [1h])) or vector(0)', 18, 6, ds=LOKI)
lg.y += 4
lg.ts('Объём логов по сервисам', [(f'sum by (service) (count_over_time({SEL} [1m]))', '{{service}}')], 0, 12, ds=LOKI, stack=True, bars=True)
lg.ts('Ошибки по сервисам', [(f'sum by (service) (count_over_time({SEL} |~ "{ERR}" [5m]))', '{{service}}')], 12, 12, ds=LOKI, stack=True, bars=True)
lg.y += 8
lg.row('Логи')
lg.logs('Все логи (фильтр: Сервис и Найти текст вверху)', f'{SEL} |~ "(?i)$search"', 0, 24, h=14)
lg.y += 14
lg.logs('Только ошибки', f'{SEL} |~ "{ERR}"', 0, 12, h=10)
lg.logs('Журнал запросов TabbyAPI', '{job="requests"} | json | line_format "{{.event}} {{.level}} {{.request_id}} {{.metrics}}"', 12, 12, h=10,
        desc='События запросов: начало, конец, причина завершения и метрики')
lg.y += 10
lg.save('llm-logs', 'LLM — логи', 'llm-logs.json', templating=VARS, links=link('llm-metrics', 'LLM — метрики'))
