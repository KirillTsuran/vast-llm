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

    def stat(self, title, targets, x, w, unit='short', color='blue', desc='', ds=PROM, mappings=None, no_value=None, decimals=0):
        """targets: an expression, or [(expr, name), ...] to show several values with their names."""
        targets = [(targets, '')] if isinstance(targets, str) else targets
        self.panels.append({'type': 'stat', 'title': title, 'description': desc, 'datasource': ds, 'gridPos': {'x': x, 'y': self.y, 'w': w, 'h': 4},
                            'targets': [{'expr': e, 'legendFormat': l, 'refId': chr(65 + i), 'instant': True, 'datasource': ds} for i, (e, l) in enumerate(targets)],
                            'fieldConfig': {'defaults': {'unit': unit, 'decimals': decimals, 'color': {'mode': 'fixed', 'fixedColor': color},
                                                         'mappings': mappings or [], **({'noValue': no_value} if no_value else {})}, 'overrides': []},
                            'options': {'reduceOptions': {'calcs': ['lastNotNull']}, 'colorMode': 'value', 'graphMode': 'none',
                                        'textMode': 'value_and_name' if len(targets) > 1 else 'value', 'orientation': 'vertical'}})

    def ts(self, title, targets, x, w, unit='short', h=9, desc='', stack=False, ds=PROM, bars=False, step=False, interval=None, decimals=None,
           colors=None):
        """Line chart; legend is a table with Last and Max, sorted by Last (largest first). colors: {series name: color}."""
        self.panels.append({'type': 'timeseries', 'title': title, 'description': desc, 'datasource': ds, 'gridPos': {'x': x, 'y': self.y, 'w': w, 'h': h},
                            'targets': [{'expr': e, 'legendFormat': l, 'refId': chr(65 + i), 'datasource': ds} for i, (e, l) in enumerate(targets)],
                            'fieldConfig': {'defaults': {'unit': unit, 'min': 0, **({'decimals': decimals} if decimals is not None else {}),
                                                         'custom': {'lineWidth': 2, 'fillOpacity': 70 if bars else 12, 'drawStyle': 'bars' if bars else 'line',
                                                                    'lineInterpolation': 'stepAfter' if step else 'smooth', 'showPoints': 'never',
                                                                    'spanNulls': False, 'stacking': {'mode': 'normal' if stack else 'none'}}},
                                            'overrides': [{'matcher': {'id': 'byName', 'options': n}, 'properties': [{'id': 'color', 'value': {'mode': 'fixed', 'fixedColor': c}}]}
                                                          for n, c in (colors or {}).items()]},
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
        (OUT / name).write_text(json.dumps(d, ensure_ascii=False, indent=1), encoding='utf-8', newline='\n')
        print(name, len(self.panels), 'panels')


UPDOWN = [{'type': 'value', 'options': {'0': {'text': 'не работает', 'color': 'red'}, '1': {'text': 'работает', 'color': 'green'}}}]
link = lambda uid, title: [{'title': title, 'type': 'link', 'url': f'/d/{uid}', 'icon': 'dashboard'}]
R = '[$__rate_interval]'

# Speed lines use the live counters (serve.py): tokens and the seconds spent on them grow during the work itself,
# so tokens / seconds is the real speed at that moment and the line is empty while the model is idle.
# The llm_generated_tokens_total-style counters grow only when an answer ends: fine for hourly averages, not for lines.
# Counts of answers come from the request journal in Loki: exactly one generation_end line per request.
ENDS = '{event="generation_end"}'
ENDS_OK = ENDS + ' !~ `"partial": true`'  # answered requests; cancelled/failed ones are logged as partial


def answers(reason_filter):
    return 'sum(count_over_time(' + ENDS_OK + ' | json reason="metrics.eos_reason" | reason' + reason_filter + ' [$__interval]))'


def answer_avg(extract, value=''):
    """Average over the answers that ended in the last 5 minutes (a line while answers come, a gap when there are none)."""
    fmt = ' | label_format v=`' + value + '`' if value else ''
    return 'avg_over_time(' + ENDS_OK + ' | json ' + extract + fmt + ' | unwrap v | __error__="" [5m]) by ()'


# ---------------- metrics ----------------
m = Dash()
m.row('Сейчас')
m.stat('Скорость, ток/с', 'sum(increase(llm_live_generated_tokens_total[1h])) / (sum(increase(llm_live_generate_seconds_total[1h])) > 0)', 0, 4,
       color='green', no_value='нет ответов',
       desc='Средняя скорость написания ответов за последний час: все написанные токены / время, пока модель писала. Простой не учитывается. '
            'Совпадает с секундомером на клиенте. В строках ответов в логах — замер самого движка, он изредка бывает ниже')
m.stat('Ответов за час', 'sum(count_over_time(' + ENDS_OK + ' [1h])) or vector(0)', 4, 4, ds=LOKI,
       desc='Сколько запросов модель довела до ответа за последний час (отменённые клиентом не считаются)')
m.stat('Запросы сейчас', [('sum(llm_requests_processing) or vector(0)', 'в работе'), ('sum(llm_requests_deferred) or vector(0)', 'ждут')], 8, 4,
       color='purple',
       desc='«В работе» — модель читает или пишет (одновременно только один запрос). «Ждут» — запросы в очереди: им ответ начнётся позже')
m.stat('Зацикливаний, сутки', 'sum(count_over_time(' + ENDS_OK + ' | json reason="metrics.eos_reason" | reason="loop_detected" [24h])) or vector(0)',
       12, 4, color='red', ds=LOKI, desc='Ответы за сутки, оборванные с ошибкой generation_loop_detected. Норма — 0')
m.stat('LoopBreak спас, сутки', 'sum(increase(llm_loopbreak_events_total[24h])) or vector(0)', 16, 4, color='orange',
       desc='Сколько раз за сутки защита прервала повтор в рассуждении, и ответ продолжился нормально')
m.stat('Загрузка GPU', 'llm_gpu_util_percent', 20, 4, unit='percent', color='yellow')
m.y += 4

m.row('Модель: скорость прямо сейчас')
m.ts('Пишет ответ, ток/с', [(f'sum(rate(llm_live_generated_tokens_total{R})) / (sum(rate(llm_live_generate_seconds_total{R})) > 0)', 'скорость')],
     0, 8, decimals=0, colors={'скорость': 'green'},
     desc='С какой скоростью модель пишет ответ в этот момент. Пусто — модель ничего не пишет. '
          'Обычно 40–140: быстрее на коротком контексте и коде, медленнее на длинном контексте')
m.ts('Читает запрос, ток/с', [(f'sum(rate(llm_live_prompt_tokens_total{R})) / (sum(rate(llm_live_prefill_seconds_total{R})) > 0)', 'скорость')],
     8, 8, decimals=0, colors={'скорость': 'blue'},
     desc='С какой скоростью модель читает новую часть запроса (то, чего нет в кэше). Пусто — модель ничего не читает. Обычно 400–1000')
# the engine's own job counts: running (reading or writing) and waiting for their turn
m.ts('Запросы: в работе и в очереди', [('max(max_over_time(llm_requests_processing' + R + '))', 'в работе'),
                                       ('max(max_over_time(llm_requests_deferred' + R + '))', 'ждут в очереди')],
     16, 8, step=True, decimals=0, colors={'в работе': 'purple', 'ждут в очереди': 'orange'},
     desc='Модель отвечает строго по одному запросу (max_batch_size 1). Если клиент шлёт несколько запросов сразу, остальные ждут в очереди')
m.y += 9

m.row('Модель: ответы')
m.ts('Ожидание до первого токена, с', [(answer_avg('q="metrics.queue_time", p="metrics.prompt_time"', '{{ addf .q .p }}'), 'всего'),
                                       (answer_avg('v="metrics.queue_time"'), 'из них в очереди')],
     0, 8, unit='s', decimals=1, interval='1m', ds=LOKI, colors={'всего': 'blue', 'из них в очереди': 'orange'},
     desc='Среднее по ответам за последние 5 минут: сколько секунд прошло от запроса до начала ответа. '
          'Складывается из ожидания в очереди и чтения запроса')
REASONS = [('="stop_token"', 'нормально завершён', 'green'), ('="max_new_tokens"', 'упёрся в лимит длины', 'yellow'),
           ('="loop_detected"', 'оборван: зацикливание', 'red'), ('!~"stop_token|max_new_tokens|loop_detected"', 'прочее', 'purple')]
m.ts('Ответы по итогу, штук',
     [(answers(f), name) for f, name, _ in REASONS] +
     [('sum(count_over_time(' + ENDS + ' |= `"partial": true` [$__interval]))', 'отменён клиентом')],
     8, 8, stack=True, bars=True, interval='5m', decimals=0, ds=LOKI, colors={**{n: c for _, n, c in REASONS}, 'отменён клиентом': 'text'},
     desc='Каждый столбик — сколько запросов закончилось за его промежуток времени и чем. '
          '«Отменён клиентом» — клиент закрыл соединение до конца ответа (например, не дождался очереди)')
m.ts('DFlash2 угадал токенов, %',
     [('100 * sum(increase(llm_draft_accepted_tokens_total[10m])) / ((sum(increase(llm_draft_accepted_tokens_total[10m])) '
       '+ sum(increase(llm_draft_rejected_tokens_total[10m]))) > 0)', 'угадано за 10 мин')],
     16, 8, unit='percent', decimals=0, colors={'угадано за 10 мин': 'green'},
     desc='Доля черновых токенов маленькой модели-помощника, которые основная модель приняла (по ответам за 10 минут). '
          'Чем выше, тем быстрее ответ. Обычно 25–75%')
m.y += 9

m.row('Модель: память и кэш')
m.ts('Размер текущего запроса, токенов', [('max(max_over_time(llm_cache_used_tokens' + R + '))', 'текущий запрос'),
                                          ('max(llm_context_tokens)', 'лимит одного запроса')],
     0, 8, decimals=0, colors={'текущий запрос': 'blue', 'лимит одного запроса': 'red'},
     desc='Сколько токенов занимает запрос, который модель сейчас обрабатывает (весь диалог с ответом). '
          'Когда подходит к лимиту, диалог надо сжимать, иначе запрос не поместится')
m.ts('Кэш запросов, токенов', [('max(llm_cache_reusable_tokens)', 'в видеопамяти'), ('max(llm_cache_capacity_tokens)', 'вместимость видеопамяти'),
                               ('max(llm_cache_ram_reusable_tokens)', 'в RAM'), ('max(llm_cache_ram_capacity_tokens)', 'вместимость RAM')],
     8, 8, decimals=0,
     colors={'в видеопамяти': 'green', 'вместимость видеопамяти': 'dark-green', 'в RAM': 'blue', 'вместимость RAM': 'dark-blue'},
     desc='Сколько уже прочитанного текста модель держит, чтобы не читать его заново в следующем запросе того же диалога')
m.ts('Запрос взят из кэша, %', [('100 * sum(increase(llm_cache_pages_reused_total[10m])) / (sum(increase(llm_cache_pages_total[10m])) > 0)', 'всего'),
                                ('100 * sum(increase(llm_cache_pages_from_ram_total[10m])) / (sum(increase(llm_cache_pages_total[10m])) > 0)', 'из них из RAM')],
     16, 8, unit='percent', decimals=0, colors={'всего': 'green', 'из них из RAM': 'blue'},
     desc='Какая часть запросов за 10 минут не читалась заново, а взялась из кэша. Чем выше, тем быстрее начинается ответ')
m.y += 9

m.row('Видеокарта')
m.ts('Видеопамять', [('llm_gpu_mem_used_mib', 'занято'), ('llm_gpu_mem_total_mib', 'всего')], 0, 8, unit='mbytes',
     colors={'занято': 'blue', 'всего': 'red'})
m.ts('Загрузка GPU, %', [('llm_gpu_util_percent', 'загрузка')], 8, 8, unit='percent', colors={'загрузка': 'yellow'})
m.ts('Температура GPU', [('llm_gpu_temp_celsius', 'температура')], 16, 8, unit='celsius', colors={'температура': 'orange'},
     desc='Выше ~83 °C видеокарта сама снижает частоту, и генерация замедляется')
m.y += 9
m.ts('Мощность GPU', [('llm_gpu_power_watts', 'потребляет'), ('llm_gpu_power_limit_watts', 'лимит')], 0, 12, unit='watt',
     colors={'потребляет': 'purple', 'лимит': 'red'})
m.ts('Частота ядра GPU', [('llm_gpu_sm_clock_mhz', 'сейчас'), ('llm_gpu_sm_clock_max_mhz', 'максимум')], 12, 12, unit='suffix: МГц', decimals=0,
     colors={'сейчас': 'green', 'максимум': 'red'},
     desc='Если под нагрузкой частота заметно ниже максимума — видеокарта ограничена по мощности или температуре')
m.y += 9

m.row('Контейнер: память, процессор, диск, сеть (в пределах лимитов Vast)')
m.ts('Оперативная память', [('llm_container_memory_used_bytes', 'занято всего'), ('llm_container_memory_programs_bytes', 'из них программы'),
                            ('llm_container_memory_limit_bytes', 'лимит контейнера')], 0, 8, unit='bytes',
     colors={'занято всего': 'blue', 'из них программы': 'purple', 'лимит контейнера': 'red'},
     desc='Память нашего контейнера (не всего сервера Vast). «Программы» нельзя освободить; остальное — файловый кэш, ядро отдаёт его само. '
          'Если «программы» дойдут до лимита, процесс будет убит')
m.ts('Процессор, ядер', [(f'rate(llm_container_cpu_seconds_total{R})', 'занято'), ('llm_container_cpu_limit_cores', 'лимит контейнера')],
     8, 8, decimals=1, colors={'занято': 'blue', 'лимит контейнера': 'red'}, desc='Сколько ядер процессора занимает контейнер')
m.ts('Диск /, занято', [('node_filesystem_size_bytes{mountpoint="/"} - node_filesystem_avail_bytes{mountpoint="/"}', 'занято'),
                        ('node_filesystem_size_bytes{mountpoint="/"}', 'всего')], 16, 8, unit='bytes', colors={'занято': 'blue', 'всего': 'red'})
m.y += 9
m.ts('Диск: чтение и запись', [(f'rate(llm_container_disk_read_bytes_total{R})', 'чтение'), (f'rate(llm_container_disk_written_bytes_total{R})', 'запись')],
     0, 12, unit='Bps', colors={'чтение': 'green', 'запись': 'orange'})
m.ts('Сеть', [(f'sum(rate(node_network_receive_bytes_total{{device!="lo"}}{R}))', 'приём'),
              (f'sum(rate(node_network_transmit_bytes_total{{device!="lo"}}{R}))', 'отдача')], 12, 12, unit='Bps',
     colors={'приём': 'green', 'отдача': 'orange'}, desc='Сеть контейнера: приём — в основном скачивание модели при старте')
m.y += 9

m.row('Сервисы')
SERVICES = [('tabby', 'TabbyAPI (модель)'), ('gpu', 'Экспортёр GPU'), ('node', 'node_exporter'), ('prometheus', 'Prometheus'),
            ('loki', 'Loki'), ('alloy', 'Alloy')]
for i, (job, title) in enumerate(SERVICES):
    # max over 1 minute: one slow scrape while the model is busy is not an outage
    m.stat(title, f'max(max_over_time(up{{job="{job}"}}[1m])) or vector(0)', i * 3, 3, mappings=UPDOWN)
m.stat('Перезапусков модели, сутки', 'sum(count_over_time({service="tabby"} |= "TabbyAPI exited" [24h])) or vector(0)', 18, 3, color='red', ds=LOKI,
       desc='Сколько раз сервер модели падал и был перезапущен автоматически. Причина — в логах tabby перед строкой «TabbyAPI exited»')
m.stat('Убито по памяти, сутки', 'sum(increase(llm_container_oom_kills_total[24h])) or vector(0)', 21, 3, color='red',
       desc='Сколько процессов ядро убило из-за нехватки памяти контейнера за сутки. Норма — 0')
m.y += 4
m.save('llm-metrics', 'LLM — метрики', 'llm-metrics.json', links=link('llm-logs', 'LLM — логи'))

# ---------------- logs ----------------
lg = Dash()
VARS = [{'type': 'query', 'name': 'service', 'label': 'Сервис', 'datasource': LOKI, 'query': {'label': 'service', 'type': 1, 'stream': '', 'refId': 'svc'},
         'definition': 'label_values(service)', 'includeAll': True, 'multi': True, 'current': {'text': 'All', 'value': '$__all'}, 'refresh': 2},
        {'type': 'textbox', 'name': 'search', 'label': 'Найти текст', 'query': '', 'current': {'text': '', 'value': ''}}]
# full request/response texts stay in Loki (event=request_body/response_body) but are hidden from the overview panels
SEL = '{service=~"$service", event!~"request_body|response_body"}'
ERR = ' | detected_level=~"error|critical|fatal"'  # Loki detects the level of every line (level= fields, ERROR keywords)
lg.row('Обзор')
lg.stat('Ответов за час', 'sum(count_over_time(' + ENDS_OK + ' [1h])) or vector(0)', 0, 6, ds=LOKI)
lg.stat('Ошибок в логах за час', f'sum(count_over_time({SEL}{ERR} [1h])) or vector(0)', 6, 6, color='red', ds=LOKI,
        desc='Строки с уровнем error, critical или fatal во всех сервисах, включая оборванные зацикливанием ответы')
lg.stat('Зацикливаний, сутки', 'sum(count_over_time(' + ENDS_OK + ' | json reason="metrics.eos_reason" | reason="loop_detected" [24h])) or vector(0)',
        12, 6, color='orange', ds=LOKI)
lg.stat('Строк логов за 15 мин', f'sum(count_over_time({SEL} [15m])) or vector(0)', 18, 6, ds=LOKI)
lg.y += 4
ITOG = ('{{ if .metrics_partial }}отменён клиентом после {{ .metrics_observed_output_tokens }} ток'
        '{{ else }}{{ if eq .metrics_eos_reason "stop_token" }}готов{{ else if eq .metrics_eos_reason "max_new_tokens" }}упёрся в лимит длины'
        '{{ else if eq .metrics_eos_reason "loop_detected" }}ОБОРВАН: зацикливание{{ else }}{{ .metrics_eos_reason }}{{ end }}'
        ' · запрос {{ .metrics_prompt_tokens }} ток (новых {{ subf .metrics_prompt_tokens .metrics_cached_tokens }})'
        ' · очередь {{ .metrics_queue_time }} с · чтение {{ .metrics_prompt_time }} с'
        ' · ответ {{ .metrics_gen_tokens }} ток за {{ .metrics_gen_time }} с ({{ .metrics_gen_tokens_per_sec }} ток/с){{ end }}')
lg.logs('Ответы модели (одна строка на запрос)', ENDS + ' | json | line_format `' + ITOG + '`', 0, 24, h=9,
        desc='Итог каждого запроса: сколько ждал в очереди, сколько читал, сколько и как быстро писал ответ')
lg.y += 9
lg.ts('Объём логов по сервисам, строк', [(f'sum by (service) (count_over_time({SEL} [$__interval]))', '{{service}}')], 0, 12, ds=LOKI, stack=True, bars=True,
      interval='1m', decimals=0)
lg.ts('Ошибки по сервисам, строк', [(f'sum by (service) (count_over_time({SEL}{ERR} [$__interval]))', '{{service}}')], 12, 12, ds=LOKI, stack=True, bars=True,
      interval='5m', decimals=0)
lg.y += 9
lg.row('Логи')
lg.logs('Только ошибки', SEL + ERR, 0, 24, h=9)
lg.y += 9
lg.logs('Все логи (вверху: выбрать Сервис и Найти текст)', f'{SEL} |~ "(?i)$search"', 0, 24, h=14)
lg.y += 14
lg.save('llm-logs', 'LLM — логи', 'llm-logs.json', templating=VARS, links=link('llm-metrics', 'LLM — метрики'))
