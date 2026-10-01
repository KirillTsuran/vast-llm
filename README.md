# VastLLM

Окно Windows с кнопками **Up** / **Down**: арендует GPU на [Vast.ai](https://vast.ai), поднимает на нём OpenAI-совместимый API
(TabbyAPI + ExLlamaV3, Qwen3.8-27B) и даёт его на этом ПК по адресу `http://127.0.0.1:8080/v1`.

## Установка

1. Скачать `VastLLM.exe` из [Releases](../../releases/latest) в любую папку (нужен .NET 10 Desktop Runtime).
2. Запустить один раз — рядом появится `config.json`. Вписать `vast_api_key` (console.vast.ai → Keys).
3. **Up** → ~9 минут → «Работает». Клиент (ZCode и т.п.): OpenAI-compatible, URL `http://127.0.0.1:8080/v1`,
   модель `qwen3.8-27b-uncensored`, ключ любой.
4. **Down** — машина удаляется, оплата прекращается. Машины удаляются **только** кнопкой Down
   (или «Да» в вопросе при закрытии окна).

## Файлы рядом с exe

| Файл | Что это |
|---|---|
| `config.json` | ключ Vast и настройки: `gpu`, `max_price_usd_per_hour`, `datacenter_only`, `race_two_hosts`, `local_port`, `image`, `model`, `usd_rub` |
| `state.json` | текущая машина (id, цена, ключ хоста) — после перезапуска программа подключается к ней снова |
| `ssh_key.pem` | SSH-ключ программы; публичная часть передаётся в контейнер при аренде |
| `vast-llm.log` | журнал |

## Как устроено

- `app/` — C# WinForms. Vast API: поиск (проверенные хосты, надёжность ≥ 98 %, сеть ≥ 500 Мбит/с, потолок цены),
  аренда двух машин параллельно с выбором первой поднявшейся (`race_two_hosts`), SSH-туннель
  `127.0.0.1:8080 → машина:127.0.0.1:8080` (SSH.NET), переподключение, цены в рублях по курсу ЦБ, сворачивание в трей.
- `image/` — образ `ghcr.io/kirilltsuran/vast-llm`: TabbyAPI (база 816c321 + production-правки), ExLlamaV3 1.5.3,
  стадия сэмплера `SS_LoopBreak` против зацикливания. Свой sshd (вход только по ключу программы); API слушает только
  `127.0.0.1` внутри контейнера. Веса скачиваются при старте по закреплённым ревизиям с проверкой SHA256.
- Сборка: `image.yml` — образ при изменениях в `image/`; `release.yml` — `VastLLM.exe` по тегу `v*`.

Лицензия кода TabbyAPI — AGPL-3.0 (исходники в `image/tabby-source.tgz` и `image/overlay`).
