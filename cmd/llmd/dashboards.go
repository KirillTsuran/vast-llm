package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

type obj = map[string]any

var (
	prom = obj{"type": "prometheus", "uid": "prom"}
	loki = obj{"type": "loki", "uid": "loki"}
)

// q is one query of a panel and the name of its series.
type q struct{ expr, name string }

// look is everything optional about a panel; the zero value is a blue Prometheus panel.
type look struct {
	unit, color, desc, noValue, interval string
	logs                                 bool // Loki instead of Prometheus
	decimals                             int
	stack, bars, step                    bool
	colors                               map[string]string // series name -> color
	mappings                             []obj
}

func (l look) source() obj {
	if l.logs {
		return loki
	}
	return prom
}

type board struct {
	panels []obj
	y      int
}

func (b *board) add(kind, title string, x, w, h int, l look, body obj) {
	p := obj{"type": kind, "title": title, "description": l.desc, "datasource": l.source(), "gridPos": obj{"x": x, "y": b.y, "w": w, "h": h}}
	for k, v := range body {
		p[k] = v
	}
	b.panels = append(b.panels, p)
}

func (b *board) row(title string) {
	b.add("row", title, 0, 24, 1, look{}, obj{"collapsed": false, "panels": []obj{}})
	b.y++
}

func targets(l look, instant bool, qs []q) []obj {
	var out []obj
	for i, t := range qs {
		out = append(out, obj{"expr": t.expr, "legendFormat": t.name, "refId": string(rune('A' + i)), "instant": instant, "datasource": l.source()})
	}
	return out
}

// stat is a row of big numbers, 4 units high; several queries show their names.
func (b *board) stat(title string, x, w int, l look, qs ...q) {
	if l.unit == "" {
		l.unit = "short"
	}
	if l.color == "" {
		l.color = "blue"
	}
	defaults := obj{"unit": l.unit, "decimals": l.decimals, "color": obj{"mode": "fixed", "fixedColor": l.color}, "mappings": l.mappings}
	if l.noValue != "" {
		defaults["noValue"] = l.noValue
	}
	text := "value"
	if len(qs) > 1 {
		text = "value_and_name"
	}
	b.add("stat", title, x, w, 4, l, obj{"targets": targets(l, true, qs), "fieldConfig": obj{"defaults": defaults, "overrides": []obj{}},
		"options": obj{"reduceOptions": obj{"calcs": []string{"lastNotNull"}}, "colorMode": "value", "graphMode": "none", "textMode": text, "orientation": "vertical"}})
}

// lines is a chart 9 units high; its legend is a table with Last and Max, largest Last first.
func (b *board) lines(title string, x, w int, l look, qs ...q) {
	if l.unit == "" {
		l.unit = "short"
	}
	draw, fill, curve, stacking := "line", 12, "smooth", "none"
	if l.bars {
		draw, fill = "bars", 70
	}
	if l.step {
		curve = "stepAfter"
	}
	if l.stack {
		stacking = "normal"
	}
	overrides := []obj{} // by series name: a query like {{finish}} yields names that are not in qs
	names := make([]string, 0, len(l.colors))
	for name := range l.colors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		overrides = append(overrides, obj{"matcher": obj{"id": "byName", "options": name},
			"properties": []obj{{"id": "color", "value": obj{"mode": "fixed", "fixedColor": l.colors[name]}}}})
	}
	body := obj{"targets": targets(l, false, qs),
		"fieldConfig": obj{"defaults": obj{"unit": l.unit, "min": 0, "decimals": l.decimals, "custom": obj{"lineWidth": 2, "fillOpacity": fill,
			"drawStyle": draw, "lineInterpolation": curve, "showPoints": "never", "spanNulls": false, "stacking": obj{"mode": stacking}}}, "overrides": overrides},
		"options": obj{"legend": obj{"displayMode": "table", "placement": "bottom", "calcs": []string{"lastNotNull", "max"}, "sortBy": "Last *", "sortDesc": true},
			"tooltip": obj{"mode": "multi", "sort": "desc"}}}
	if l.interval != "" {
		body["interval"] = l.interval
	}
	b.add("timeseries", title, x, w, 9, l, body)
}

func (b *board) logLines(title, expr string, h int) {
	b.add("logs", title, 0, 24, h, look{logs: true}, obj{"targets": []obj{{"expr": expr, "refId": "A", "datasource": loki}},
		"options": obj{"showTime": true, "wrapLogMessage": true, "sortOrder": "Descending", "enableLogDetails": true, "dedupStrategy": "none"}})
	b.y += h
}

func (b *board) json(uid, title, otherUID, otherTitle string, vars []obj) []byte {
	raw, err := json.MarshalIndent(obj{"uid": uid, "title": title, "timezone": "browser", "refresh": "10s", "time": obj{"from": "now-1h", "to": "now"},
		"schemaVersion": 39, "version": 1, "editable": true, "panels": b.panels, "templating": obj{"list": vars}, "annotations": obj{"list": []obj{}},
		"links": []obj{{"title": otherTitle, "type": "link", "url": "/d/" + otherUID, "icon": "dashboard"}}}, "", " ")
	if err != nil {
		panic(err) // only literals above: cannot fail
	}
	return raw
}

// dashboards builds the two Grafana dashboards: file name -> JSON.
func dashboards() map[string][]byte {
	const rate = "[$__rate_interval]"
	upDown := []obj{{"type": "value", "options": obj{"0": obj{"text": "не работает", "color": "red"}, "1": obj{"text": "работает", "color": "green"}}}}
	share := func(part, whole string) string { // percent over 10 minutes, empty while nothing happens
		return fmt.Sprintf("100 * sum(increase(%s[10m])) / (sum(increase(%s[10m])) > 0)", part, whole)
	}

	m := &board{}
	m.row("Сейчас")
	m.stat("Скорость одного запроса, ток/с", 0, 4, look{color: "green", noValue: "нет ответов",
		desc: "Средняя скорость написания ответа за последний час по завершённым запросам: написанные токены / время запроса без чтения. " +
			"Когда запросы идут одновременно, каждый пишет медленнее, и это число падает"},
		q{"sum(increase(llm_request_output_tokens_total[1h])) / (sum(increase(llm_request_seconds_total[1h])) - sum(increase(llm_request_prompt_seconds_total[1h])) > 0)", ""})
	m.stat("Ответов за час", 4, 4, look{}, q{"round(sum(increase(llm_requests_total[1h]))) or vector(0)", ""})
	m.stat("Запросы сейчас", 8, 4, look{color: "purple",
		desc: "«В работе» — запросы, которые модель читает или пишет прямо сейчас. «Ждут» — запросы, до которых очередь ещё не дошла"},
		q{"sum(llm_requests_processing) or vector(0)", "в работе"}, q{"sum(llm_requests_queued) or vector(0)", "ждут"})
	m.stat("Оборвано по длине, сутки", 12, 4, look{color: "red",
		desc: "Ответы, которые закончились не сами: упёрлись в лимит длины или движок оборвал повтор одного токена. Много таких подряд — признак зацикливания"},
		q{`round(sum(increase(llm_finished_total{finish="length"}[24h]))) or vector(0)`, ""})
	m.stat("Запас памяти", 16, 4, look{unit: "bytes", color: "orange", decimals: 1,
		desc: "Сколько памяти контейнера ещё не занято программами (лимит минус программы). Каждый одновременный запрос держит свой контекст в памяти. " +
			"Если запас дойдёт до нуля, ядро убьёт движок"},
		q{"min(llm_container_memory_limit_bytes - llm_container_memory_programs_bytes)", ""})
	m.stat("Перезапусков движка, сутки", 20, 4, look{color: "red",
		desc: "Сколько раз движок падал и был запущен снова. Причина — в логах strata и engine перед строкой «exited»"},
		q{"round(sum(increase(llm_engine_restarts_total[24h]))) or vector(0)", ""})
	m.y += 4

	m.row("Модель: скорость и одновременные запросы")
	m.lines("Пишет ответ, ток/с", 0, 8, look{colors: map[string]string{"все запросы вместе": "green", "в среднем на запрос": "blue"},
		desc: "С какой скоростью модель пишет в этот момент: суммарно по всем одновременным запросам и в среднем на один. " +
			"Когда запросов несколько, каждый идёт медленнее, чем шёл бы один. Пусто — модель ничего не пишет"},
		q{"llm_live_tok_s > 0", "все запросы вместе"}, q{"llm_live_tok_s_per_request > 0", "в среднем на запрос"})
	m.lines("Читает запрос, ток/с", 8, 8, look{colors: map[string]string{"скорость за 5 мин": "blue"},
		desc: "С какой скоростью модель читала новую часть запросов (то, чего нет в кэше) за последние 5 минут. Пусто — новых запросов не было"},
		q{"(sum(increase(llm_prompt_tokens_total[5m])) - sum(increase(llm_reused_tokens_total[5m]))) / (sum(increase(llm_prompt_seconds_total[5m])) > 0)", "скорость за 5 мин"})
	m.lines("Запросы: в работе и в очереди", 16, 8, look{step: true, colors: map[string]string{"в работе": "purple", "ждут": "orange", "слотов всего": "red"},
		desc: "Сколько запросов модель обрабатывает одновременно и сколько ждут. «Слотов всего» — предел одновременных запросов, остальные ждут в очереди"},
		q{"max(max_over_time(llm_requests_processing" + rate + "))", "в работе"}, q{"max(max_over_time(llm_requests_queued" + rate + "))", "ждут"},
		q{"max(llm_slots_total)", "слотов всего"})
	m.y += 9

	m.row("Модель: ответы и кэш")
	m.lines("Ответы по итогу, штук", 0, 8, look{stack: true, bars: true, interval: "5m",
		colors: map[string]string{"stop": "green", "length": "red", "cancel": "text", "disconnect": "orange", "error": "dark-red", "unknown": "purple"},
		desc: "Сколько запросов закончилось за промежуток и чем: stop — модель закончила сама, length — оборвано по длине или из-за повтора, " +
			"cancel и disconnect — клиент отменил или отключился, unknown — ответ закончился, но причина не попала в метрики"},
		q{"round(sum by (finish) (increase(llm_finished_total[$__interval])))", "{{finish}}"})
	m.lines("Черновики, %", 8, 8, look{unit: "percent", colors: map[string]string{"принято из предложенных": "green", "доля ответа из черновиков": "blue"},
		desc: "Черновики ускоряют только запрос, который идёт один: при одновременных запросах движок их не использует. " +
			"«Доля ответа из черновиков» падает, когда запросы идут вместе, — это и есть цена одновременной работы"},
		q{share("llm_drafts_accepted_total", "llm_drafts_offered_total"), "принято из предложенных"},
		q{share("llm_drafts_accepted_total", "llm_output_tokens_total"), "доля ответа из черновиков"})
	m.lines("Запрос взят из кэша, %", 16, 8, look{unit: "percent", colors: map[string]string{"из кэша за 10 мин": "green"},
		desc: "Какая часть токенов запросов за 10 минут не читалась заново, а взялась из кэша диалога. Чем выше, тем быстрее начинается ответ"},
		q{share("llm_reused_tokens_total", "llm_prompt_tokens_total"), "из кэша за 10 мин"})
	m.y += 9

	m.row("Модель: долгие запросы")
	m.lines("Самый долгий запрос в работе, секунд", 0, 12, look{unit: "s", colors: map[string]string{"идёт уже": "orange"},
		desc: "Сколько времени идёт самый долгий из запросов, которые модель обрабатывает сейчас. Растёт десятки минут — возможно, ответ зациклился"},
		q{"max(max_over_time(llm_live_longest_request_seconds" + rate + ")) > 0", "идёт уже"})
	m.lines("Самый длинный ответ в работе, токенов", 12, 12, look{colors: map[string]string{"написано": "orange"},
		desc: "Сколько токенов (рассуждение и текст) уже написал самый длинный из текущих ответов. Десятки тысяч без конца — признак зацикливания"},
		q{"max(max_over_time(llm_live_longest_answer_tokens" + rate + ")) > 0", "написано"})
	m.y += 9

	m.row("Модель: контекст и видеопамять движка")
	m.lines("Размер текущего запроса, токенов", 0, 12, look{colors: map[string]string{"текущий запрос": "blue", "окно контекста": "red"},
		desc: "Сколько токенов занимает самый большой из запросов, которые модель сейчас обрабатывает (запрос и уже написанный ответ). У окна контекста диалог надо сжимать"},
		q{"max(max_over_time(llm_live_request_tokens" + rate + "))", "текущий запрос"}, q{"max(llm_context_tokens)", "окно контекста"})
	m.lines("Видеопамять движка", 12, 12, look{unit: "mbytes", colors: map[string]string{"кэш экспертов": "blue", "свободно после загрузки": "green"},
		desc: "Кэш экспертов — части модели, которые движок держит на GPU. Свободно — сколько осталось после загрузки всего"},
		q{"llm_expert_cache_mib", "кэш экспертов"}, q{"llm_vram_free_mib", "свободно после загрузки"})
	m.y += 9

	m.row("Видеокарта")
	m.lines("Видеопамять", 0, 8, look{unit: "mbytes", colors: map[string]string{"занято": "blue", "всего": "red"}},
		q{"llm_gpu_mem_used_mib", "занято"}, q{"llm_gpu_mem_total_mib", "всего"})
	m.lines("Загрузка GPU, %", 8, 8, look{unit: "percent", colors: map[string]string{"загрузка": "yellow"}}, q{"llm_gpu_util_percent", "загрузка"})
	m.lines("Температура GPU", 16, 8, look{unit: "celsius", colors: map[string]string{"температура": "orange"},
		desc: "Выше ~83 °C видеокарта сама снижает частоту, и генерация замедляется"}, q{"llm_gpu_temp_celsius", "температура"})
	m.y += 9
	m.lines("Мощность GPU", 0, 12, look{unit: "watt", colors: map[string]string{"потребляет": "purple", "лимит": "red"}},
		q{"llm_gpu_power_watts", "потребляет"}, q{"llm_gpu_power_limit_watts", "лимит"})
	m.lines("Частота ядра GPU", 12, 12, look{unit: "suffix: МГц", colors: map[string]string{"сейчас": "green", "максимум": "red"},
		desc: "Если под нагрузкой частота заметно ниже максимума — видеокарта ограничена по мощности или температуре"},
		q{"llm_gpu_sm_clock_mhz", "сейчас"}, q{"llm_gpu_sm_clock_max_mhz", "максимум"})
	m.y += 9

	m.row("Контейнер: память, процессор, диск, сеть (в пределах лимитов контейнера)")
	m.lines("Оперативная память", 0, 8, look{unit: "bytes", colors: map[string]string{"занято всего": "blue", "из них программы": "purple", "лимит контейнера": "red"},
		desc: "Память нашего контейнера (не всего сервера). «Программы» нельзя освободить; остальное — файловый кэш, ядро отдаёт его само. " +
			"Если «программы» дойдут до лимита, процесс будет убит"},
		q{"llm_container_memory_used_bytes", "занято всего"}, q{"llm_container_memory_programs_bytes", "из них программы"},
		q{"llm_container_memory_limit_bytes", "лимит контейнера"})
	m.lines("Процессор, ядер", 8, 8, look{decimals: 1, colors: map[string]string{"занято": "blue", "лимит контейнера": "red"}},
		q{"rate(llm_container_cpu_seconds_total" + rate + ")", "занято"}, q{"llm_container_cpu_limit_cores", "лимит контейнера"})
	m.lines("Диск /, занято", 16, 8, look{unit: "bytes", colors: map[string]string{"занято": "blue", "всего": "red"}},
		q{`node_filesystem_size_bytes{mountpoint="/"} - node_filesystem_avail_bytes{mountpoint="/"}`, "занято"},
		q{`node_filesystem_size_bytes{mountpoint="/"}`, "всего"})
	m.y += 9
	m.lines("Диск: чтение и запись", 0, 12, look{unit: "Bps", colors: map[string]string{"чтение": "green", "запись": "orange"}},
		q{"rate(llm_container_disk_read_bytes_total" + rate + ")", "чтение"}, q{"rate(llm_container_disk_written_bytes_total" + rate + ")", "запись"})
	m.lines("Сеть", 12, 12, look{unit: "Bps", colors: map[string]string{"приём": "green", "отдача": "orange"},
		desc: "Сеть контейнера: приём — в основном скачивание модели при старте"},
		q{`sum(rate(node_network_receive_bytes_total{device!="lo"}` + rate + "))", "приём"},
		q{`sum(rate(node_network_transmit_bytes_total{device!="lo"}` + rate + "))", "отдача"})
	m.y += 9

	m.row("Сервисы")
	// max over 1 minute: one slow scrape while the model is busy is not an outage
	m.stat("Движок Strata", 0, 3, look{mappings: upDown}, q{"max(max_over_time(llm_engine_up[1m])) or vector(0)", ""})
	for i, s := range []struct{ job, title string }{{"llmd", "llmd (экспортёр)"}, {"node", "node_exporter"}, {"prometheus", "Prometheus"}, {"loki", "Loki"}, {"alloy", "Alloy"}} {
		m.stat(s.title, (i+1)*3, 3, look{mappings: upDown}, q{fmt.Sprintf(`max(max_over_time(up{job=%q}[1m])) or vector(0)`, s.job), ""})
	}
	m.stat("Убито по памяти, сутки", 18, 6, look{color: "red", desc: "Сколько процессов ядро убило из-за нехватки памяти контейнера за сутки. Норма — 0"},
		q{"sum(increase(llm_container_oom_kills_total[24h])) or vector(0)", ""})
	m.y += 4

	const sel = `{service=~"$service"}`
	const errs = ` | detected_level=~"error|critical|fatal"` // Loki detects the level of every line
	l := &board{}
	l.row("Обзор")
	l.stat("Ошибок в логах за час", 0, 12, look{logs: true, color: "red", desc: "Строки с уровнем error, critical или fatal во всех сервисах"},
		q{"sum(count_over_time(" + sel + errs + " [1h])) or vector(0)", ""})
	l.stat("Строк логов за 15 мин", 12, 12, look{logs: true}, q{"sum(count_over_time(" + sel + " [15m])) or vector(0)", ""})
	l.y += 4
	l.lines("Объём логов по сервисам, строк", 0, 12, look{logs: true, stack: true, bars: true, interval: "1m"},
		q{"sum by (service) (count_over_time(" + sel + " [$__interval]))", "{{service}}"})
	l.lines("Ошибки по сервисам, строк", 12, 12, look{logs: true, stack: true, bars: true, interval: "5m"},
		q{"sum by (service) (count_over_time(" + sel + errs + " [$__interval]))", "{{service}}"})
	l.y += 9
	l.row("Логи")
	l.logLines("Запросы к модели (строка движка на каждый запрос: прочитано, написано, скорость)", `{service="engine"} |= "strata serve: prompt"`, 9)
	l.logLines("Только ошибки", sel+errs, 9)
	l.logLines("Все логи (вверху: выбрать Сервис и Найти текст)", sel+` |~ "(?i)$search"`, 14)
	vars := []obj{
		{"type": "query", "name": "service", "label": "Сервис", "datasource": loki, "query": obj{"label": "service", "type": 1, "stream": "", "refId": "svc"},
			"definition": "label_values(service)", "includeAll": true, "multi": true, "current": obj{"text": "All", "value": "$__all"}, "refresh": 2},
		{"type": "textbox", "name": "search", "label": "Найти текст", "query": "", "current": obj{"text": "", "value": ""}},
	}
	return map[string][]byte{
		"llm-metrics.json": m.json("llm-metrics", "LLM — метрики", "llm-logs", "LLM — логи", []obj{}),
		"llm-logs.json":    l.json("llm-logs", "LLM — логи", "llm-metrics", "LLM — метрики", vars),
	}
}
