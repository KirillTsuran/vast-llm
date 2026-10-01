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

    def ts(self, title, targets, x, w, unit='short', h=10, desc='', stack=False, ds=PROM, bars=False, interval=None):
        self.panels.append({'type': 'timeseries', 'title': title, 'description': desc, 'datasource': ds, 'gridPos': {'x': x, 'y': self.y, 'w': w, 'h': h},
                            'targets': [{'expr': e, 'legendFormat': l, 'refId': chr(65 + i), 'datasource': ds} for i, (e, l) in enumerate(targets)],
                            'fieldConfig': {'defaults': {'unit': unit, 'custom': {'lineWidth': 2, 'fillOpacity': 60 if bars else 10,
                                                                                  'drawStyle': 'bars' if bars else 'line',
                                                                                  'stacking': {'mode': 'normal' if stack else 'none'}}}, 'overrides': []},
                            # legend as a table: Last and Max, sorted by Last (largest first)
                            'options': {'legend': {'displayMode': 'table', 'placement': 'bottom', 'calcs': ['lastNotNull', 'max'],
                                                   'sortBy': 'Last *', 'sortDesc': True},
                                        'tooltip': {'mode': 'multi', 'sort': 'desc'}},
                            **({'interval': interval} if interval else {})})

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
m.stat('Скорость, ток/с', 'sum(increase(llm_generated_tokens_total[1h])) / clamp_min(sum(increase(llm_decode_seconds_total[1h])), 0.001)', 0, 4, color='green',
       desc='Сколько токенов в секунду пишет модель: среднее за последний час, только пока она пишет (простой не учитывается)')
m.stat('Ответов за час', 'sum(increase(llm_requests_completed_total[1h]))', 4, 4)
m.stat('Пишет сейчас', 'sum(llm_requests_processing) or vector(0)', 8, 4, color='purple', desc='Сколько запросов модель обрабатывает прямо сейчас (0 или 1)')
m.stat('Зацикливаний за сутки', 'sum(increase(llm_requests_finished_total{reason="loop_detected"}[24h])) or vector(0)', 12, 4, color='red',
       desc='Ответы, оборванные с ошибкой generation_loop_detected. Норма — 0')
m.stat('LoopBreak спас, раз', 'sum(increase(llm_loopbreak_events_total[24h])) or vector(0)', 16, 4, color='orange',
       desc='За сутки: сколько раз защита прервала повтор в рассуждении, и ответ продолжился нормально')
m.stat('Загрузка GPU', 'llm_gpu_util_percent', 20, 4, unit='percent', color='yellow')
m.y += 4
m.row('Модель')
m.ts('Скорость, токенов в секунду', [('sum(rate(llm_generated_tokens_total[1m]))', 'пишет ответ'), ('sum(rate(llm_prompt_tokens_total[1m]))', 'читает запрос')], 0, 12,
     desc='«Читает запрос» — обработка нового текста запроса (всплески при длинных вопросах), «пишет ответ» — генерация')
REASONS = [('stop_token', 'нормально завершён'), ('max_new_tokens', 'упёрся в лимит длины'), ('loop_detected', 'оборван: зацикливание')]
m.ts('Ответы по итогу, штук за минуту',
     [(f'sum(increase(llm_requests_finished_total{{reason="{r}"}}[1m])) or vector(0)', name) for r, name in REASONS] +
     [('sum(increase(llm_requests_finished_total{reason!~"stop_token|max_new_tokens|loop_detected"}[1m])) or vector(0)', 'прочее')],
     12, 12, stack=True, bars=True, interval='1m', desc='Каждый столбик — сколько ответов закончилось за минуту и как')
m.y += 10
m.ts('Память контекста, токенов', [('llm_cache_used_tokens', 'занято сейчас'), ('llm_context_tokens', 'максимум (112K)')], 0, 12,
     desc='Сколько токенов диалога сейчас держит модель. Когда «занято» подходит к максимуму, длинный диалог надо сжимать')
m.ts('Ускорение DFlash2: угадано токенов, %', [('100 * sum(rate(llm_draft_accepted_tokens_total[2m])) / clamp_min(sum(rate(llm_draft_accepted_tokens_total[2m])) + sum(rate(llm_draft_rejected_tokens_total[2m])), 0.001)', 'угадано')],
     12, 12, unit='percent', desc='Доля токенов, которые маленькая модель-помощник угадала заранее. Чем выше, тем быстрее генерация')
m.y += 10
m.row('Видеокарта')
m.ts('Видеопамять, МиБ', [('llm_gpu_mem_used_mib', 'занято'), ('llm_gpu_mem_total_mib', 'всего')], 0, 8, unit='decmbytes')
m.ts('Загрузка GPU, %', [('llm_gpu_util_percent', 'загрузка')], 8, 8, unit='percent')
m.ts('Мощность (Вт) и температура (°C)', [('llm_gpu_power_watts', 'Вт'), ('llm_gpu_temp_celsius', '°C')], 16, 8)
m.y += 10
m.row('Сервер: процессор, память, диск, сеть')
m.ts('CPU, %', [('100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[1m])))', 'занято')], 0, 6, unit='percent')
m.ts('Оперативная память', [('node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes', 'занято'), ('node_memory_MemTotal_bytes', 'всего')], 6, 6, unit='bytes')
m.ts('Диск /, занято', [('node_filesystem_size_bytes{mountpoint="/"} - node_filesystem_avail_bytes{mountpoint="/"}', 'занято'),
                       ('node_filesystem_size_bytes{mountpoint="/"}', 'всего')], 12, 6, unit='bytes')
m.ts('Сеть', [('sum(rate(node_network_receive_bytes_total{device!="lo"}[1m]))', 'приём'), ('sum(rate(node_network_transmit_bytes_total{device!="lo"}[1m]))', 'отдача')],
     18, 6, unit='Bps')
m.y += 10
m.row('Сервисы мониторинга и модели')
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
# full request/response texts stay in Loki (event=request_body/response_body) but are hidden from the overview panels
SEL = '{service=~"$service", event!~"request_body|response_body"}'
ERR = '(?i)(level=error|level=ERROR|"level": "error"|exception|traceback|out of memory|loop_detected)'
ENDS = '{job="requests", event="generation_end"}'
lg.row('Обзор')
lg.stat('Ответов за час', f'sum(count_over_time({ENDS} [1h])) or vector(0)', 0, 6, ds=LOKI)
lg.stat('Ошибок в логах за час', f'sum(count_over_time({SEL} |~ `{ERR}` [1h])) or vector(0)', 6, 6, color='red', ds=LOKI,
        desc='Строки уровня error, exception, traceback, out of memory, loop_detected во всех сервисах')
lg.stat('Зацикливаний за сутки', f'sum(count_over_time({ENDS} |= "loop_detected" [24h])) or vector(0)', 12, 6, color='orange', ds=LOKI)
lg.stat('Строк логов за 15 мин', f'sum(count_over_time({SEL} [15m])) or vector(0)', 18, 6, ds=LOKI)
lg.y += 4
lg.logs('Ответы модели (одна строка на ответ)',
        f'{ENDS} | json | line_format `итог: {{{{.metrics_eos_reason}}}} · вход {{{{.metrics_prompt_tokens}}}} ток (из кэша {{{{.metrics_cached_tokens}}}}) · '
        f'ответ {{{{.metrics_gen_tokens}}}} ток · {{{{.metrics_gen_tokens_per_sec}}}} ток/с · всего {{{{.metrics_total_time}}}} с · контекст {{{{.context_len}}}}`',
        0, 24, h=9, desc='stop_token — ответ завершён нормально; max_new_tokens — упёрся в лимит длины; loop_detected — оборван из-за зацикливания')
lg.y += 9
lg.ts('Объём логов по сервисам, строк в минуту', [(f'sum by (service) (count_over_time({SEL} [1m]))', '{{service}}')], 0, 12, ds=LOKI, stack=True, bars=True)
lg.ts('Ошибки по сервисам, за 5 минут', [(f'sum by (service) (count_over_time({SEL} |~ `{ERR}` [5m]))', '{{service}}')], 12, 12, ds=LOKI, stack=True, bars=True)
lg.y += 10
lg.row('Логи')
lg.logs('Только ошибки', f'{SEL} |~ `{ERR}`', 0, 24, h=9)
lg.y += 9
lg.logs('Все логи (вверху: выбрать Сервис и Найти текст)', f'{SEL} |~ "(?i)$search"', 0, 24, h=14)
lg.y += 10
lg.save('llm-logs', 'LLM — логи', 'llm-logs.json', templating=VARS, links=link('llm-metrics', 'LLM — метрики'))
