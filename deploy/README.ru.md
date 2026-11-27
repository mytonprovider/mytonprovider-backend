# Тестовый деплой coordinator на VPS

**[English version](README.md)**

Стек:
- `coordinator`
- `postgres` (инициализация схемы из `db/init.sql`)
- `prometheus`
- `grafana`

## Чеклист продакшн-деплоя (порядок)

1. VPS: Docker + Compose + Task + git; клон репо, нужная ветка (`git checkout <branch>`).
2. **TLS:** CA и сертификаты агентов — [mytonprovider-agent](https://github.com/mytonprovider/mytonprovider-agent); на coordinator: `secrets/agents-ca.crt`, каталог `certs/agent-*/` для выдачи.
3. `task deploy:init` → правка `deploy/.env` (см. ниже).
3a. **Pipeline events (если обновление с новой версией):** на существующей БД выполнить SQL **до** деплоя coordinator — [db/migrations/README.ru.md](../db/migrations/README.ru.md) (`001_pipeline_events.up.sql`). Новые инсталлы получают таблицы из `init.sql` автоматически.
4. `task deploy:up` (в compose уже проброшен UDP `16167` для ADNL/DHT).
5. Smoke: `/health`, `/metrics` с Bearer (см. [secrets/README.ru.md](secrets/README.ru.md)).
6. Опционально: Tailscale на coordinator и агенты; `AGENT_ENDPOINTS` = Tailscale IP агентов.
7. На каждом агенте: новые `server.crt`/`server.key`, тот же `AGENT_AUTH_TOKEN`, `chmod 644` на ключ — [mytonprovider-agent](https://github.com/mytonprovider/mytonprovider-agent).
8. Опционально: `ufw` (не открывать `5432` наружу).
9. Опционально: фронт — [§ Фронт](#фронт-nginx).

## Место на диске (VPS)

`task deploy:up` запускает **`docker compose ... --build`**: образ coordinator **собирается на VPS** (см. `build:` у сервиса `coordinator` в [docker-compose.yml](docker-compose.yml)). Ожидайте расход диска на образ Go builder и кэш сборки — это тяжелее, чем hub-режим.

**Рекомендуется для VPS:** образ из GHCR — см. [§ GHCR](#ghcr-рекомендуется-для-vps) ниже.

## GHCR (рекомендуется для VPS)

Образ публикует задание `publish` в `CI` после проверок при каждом push в `master` (`ghcr.io/mytonprovider/mytonprovider-backend:<tag>`, тег — первые 7 символов коммита), на VPS только `pull` + `compose up` — без `golang:bookworm` и компиляции на сервере.

| Шаг | Где | Команда |
|-----|-----|---------|
| 1 | GitHub | Слияние в `master` → задание `publish` в `CI` публикует образ |
| 2 | VPS | Клон репо, `deploy/` + secrets |
| 3 | VPS | `task hub:init` → правка `deploy/.env.hub` |
| 4 | VPS | `task hub:up` |

**Зачем:** `task deploy:up` делает `docker compose ... --build`. Hub compose ([docker-compose.hub.yml](docker-compose.hub.yml)) использует `image: ${COORDINATOR_IMAGE}` и `pull_policy: always`.

### Сборка и push вручную (dev-машина, запасной путь)

```bash
docker login ghcr.io
COORDINATOR_IMAGE=ghcr.io/mytonprovider/mytonprovider-backend:<tag> task image:build:push
```

По отдельности: `task image:build`, `task image:push` (нужен `COORDINATOR_IMAGE`).

### VPS: pull + up

```bash
task hub:init
nano deploy/.env.hub
task hub:up
task hub:ps
```

В `.env.hub`:
- `COORDINATOR_IMAGE=ghcr.io/mytonprovider/mytonprovider-backend:<tag>`
- остальное — как в [`.env.example`](.env.example) (`DB_*`, `AGENT_*`, порты)

Секреты: `secrets/agents-ca.crt`, `secrets/metrics.token` — как для обычного деплоя.

Остановка: `task hub:down`.

### Задачи Task (hub)

| Task | Описание |
|------|----------|
| `image:build` | Сборка образа локально (`COORDINATOR_IMAGE`) |
| `image:push` | Push образа |
| `image:build:push` | Сборка + push |
| `hub:init` | Создать `.env.hub` из example |
| `hub:up` / `down` / `ps` / `logs` | Стек из образа |

Фронтенд — по-прежнему [§ Фронт](#фронт-nginx) (`task deploy:frontend` или ручной nginx).

### Обновление с `mytonprovider-coordinator`

Файлы compose переехали из `coordinator/deploy/` в `deploy/`: перенесите туда `.env`, `.env.hub` и `secrets/`. Проект compose теперь называется `mytonprovider-backend`, поэтому тома — `mytonprovider-backend_postgres_data`, `_prometheus_tsdb` и `_grafana_data`. На существующей установке первый `up` из этого репозитория поднимет пустую базу. Перед ним: снимите дамп базы, остановите старый стек через `down` (без `-v`), скопируйте данные из томов `mytonprovider-coordinator_*` в новые и проверьте их. Старые тома удаляйте только после этого.

## 1) Подготовка

Из корня репозитория:

```bash
task deploy:init
nano deploy/.env
```

### Обязательно в `.env`

| Переменная | Заметка |
|------------|---------|
| `DB_PASSWORD` | Сильный пароль Postgres |
| `SYSTEM_ACCESS_TOKENS` | CSV **сырых** токенов (см. secrets); первый = metrics |
| `AGENT_AUTH_TOKEN` | Общий секрет с **каждым** агентом (`AGENT_AUTH_TOKEN` в `.env.hub`) |
| `AGENT_ENDPOINTS` | CSV `host:8443` — IP **должен совпадать с SAN** в `server.crt` агента |
| `AGENT_CA_CERT_FILE` | `/run/secrets/agents-ca.crt` |
| `TON_CONFIG_URL` | Обычно `https://ton-blockchain.github.io/global.config.json` |

Пока агентов нет: `AGENT_ENDPOINTS=` (пусто). После подключения агентов — Tailscale IP или публичный IP, тот же, что в SAN сертификата.

### StoreProof: тайминги

Воркер `StoreProof` якорит полный цикл от **старта** прогона: `nextFullAt = start + INTERVAL`. Ретрай failed bags живёт внутри этого окна; в конце воркер спит только **остаток** до `nextFullAt` (не свежий час после ретрая).

| Переменная | Дефолт | Смысл |
|------------|--------|--------|
| `COORDINATOR_STOREPROOF_INTERVAL_MINUTES` | `60` | Длина полного цикла от старта `StoreProof` |
| `COORDINATOR_STOREPROOF_RETRY_DELAY_MINUTES` | `15` | Пауза после основного прогона до одного ретрая. `0` = ретрай выключен (fail’ы основного коммитятся сразу) |
| `COORDINATOR_RUNCHECKS_TOTAL_MS` | `1200000` (20 мин) | Бюджет агента на сами proof-check’ы в одном `RunChecks` |
| `AGENT_RPC_TIMEOUT_MS` | см. `.env` | Потолок ожидания gRPC ответа агента. Должен быть **≥** `RUNCHECKS_TOTAL_MS` и не съедать окно ретрая |

Хардкод: за **5 минут** до `nextFullAt` ретрай обязан остановиться (buffer на подготовку к следующему полному прогону).

**Формула окна ретрая (worst case):**

```text
retryWindow ≈ INTERVAL - mainDuration - RETRY_DELAY - 5m
```

Пример как на проде (INTERVAL=60, TotalMs≈20m, delay=15m):

```text
0 ........ 20m .............. 35m ................. 55m .... 60m
|--- main ---|---- delay ----|---- retry ≤20m ----|-buf-| next
```

Если основной прогон закончился раньше 20m — delay стартует раньше, окно ретрая больше.

**Важно:** если `AGENT_RPC_TIMEOUT_MS` сильно больше `COORDINATOR_RUNCHECKS_TOTAL_MS` (например 50m vs 20m), зависший/медленный gRPC может съесть delay и ретрай. Держите RPC timeout около TotalMs + небольшой запас (например 25m при TotalMs=20m).

Поведение записи в БД: успехи и «мёртвые» ошибки пишутся сразу; транзиентные fail’ы откладываются до ретрая (или коммитятся, если ретрай скипнут/`RETRY_DELAY=0`). Pipeline events пишутся сразу честно для отладки.

### Проверка IP провайдера

`RefreshProviderIPs` резолвит ADNL-адрес каждого провайдера в DHT и пишет `providers.ip` и `port`. Storage-эндпоинт остаётся на `StoreProof` и `RefreshEndpointsFull`. Промах DHT не затирает уже записанный IP.

| Переменная | Дефолт | Смысл |
|------------|--------|--------|
| `COORDINATOR_PROVIDER_IP_REFRESH_INTERVAL_MINUTES` | `15` | Пауза после успешного прохода |

### Секреты

См. [secrets/README.ru.md](secrets/README.ru.md):
- `agents-ca.crt` — CA, которой подписаны сертификаты агентов
- `metrics.token` — **сырой** токен для Prometheus (дубликат первого токена из `SYSTEM_ACCESS_TOKENS`)

Выпуск сертификатов на coordinator (пример):

```bash
# CA один раз
mkdir -p certs/ca
openssl genrsa -out certs/ca/ca.key 4096
openssl req -x509 -new -nodes -key certs/ca/ca.key -sha256 -days 3650 \
  -out certs/ca/ca.crt -subj "/CN=mytonprovider-root-ca"
cp certs/ca/ca.crt deploy/secrets/agents-ca.crt

# Сертификат агента: IP.1 = адрес из AGENT_ENDPOINTS (часто Tailscale 100.x.x.x)
# Подробно: README репозитория mytonprovider-agent
```

При **смене coordinator** (новая CA): перевыпустите все `server.crt`/`server.key` агентов и обновите `agents-ca.crt`. Старые сертификаты от тестового coordinator не подойдут.

## 2) Запуск

```bash
task deploy:up
```

## 3) Smoke checks

```bash
task deploy:ps
curl -fsS "http://127.0.0.1:${COORDINATOR_PORT:-8080}/health"

METRICS=$(tr -d '\n' < deploy/secrets/metrics.token)
curl -fsS -H "Authorization: Bearer ${METRICS}" \
  "http://127.0.0.1:${COORDINATOR_PORT:-8080}/metrics" | head -3
```

С Tailscale coordinator:

```bash
nc -zv <agent-tailscale-ip> 8443
```

В логах coordinator не должно быть постоянных `connection refused` / `all agents are unavailable` после того, как агенты подняты.

## 4) Логи / остановка

```bash
task deploy:logs
task deploy:down
```

## Tailscale (coordinator ↔ агенты)

Рекомендуется: coordinator и агенты в одной tailnet; в `AGENT_ENDPOINTS` — **Tailscale IPv4** агентов (`100.x.x.x:8443`), в SAN сертификата агента — **тот же** IP.

Проверка с coordinator:

```bash
tailscale ip -4
ping -c 1 <agent-ip>
nc -zv <agent-ip> 8443
```

Публичный IP агента в endpoints при сертификате только на Tailscale IP — TLS handshake не пройдёт.

## Фронт (отдельный деплой)

Каталог — репозиторий [mytonprovider-frontend](https://github.com/mytonprovider/mytonprovider-frontend), поднимается **отдельно** от coordinator.

**DNS:** `api.mytonprovider.org` → VPS (тот же хост, что и coordinator). API слушает `${COORDINATOR_PORT:-8080}`.

**CORS** в `.env.hub` — origin фронта (браузер ходит с другого поддомена):

```env
CORS_ALLOWED_ORIGINS=https://mytonprovider.org
```

**Фронт** (образ собирается с `VITE_API_URL=https://api.mytonprovider.org/api/v1`):

```bash
task hub:init   # в mytonprovider-frontend
task hub:up
```

TLS для `mytonprovider.org` и `api.mytonprovider.org` — снаружи стека (Cloudflare, certbot и т.д.).

### Старый фронт (nginx на хосте)

`task deploy:frontend` — legacy Next.js через host nginx. Для нового фронта используйте образ, как выше.

## Фронт (nginx, legacy)

После запущенного coordinator, на **той же VPS**:

**По IP (без Let's Encrypt):**

```bash
DOMAIN=<vps-public-ip> \
PUBLIC_ORIGIN=http://<vps-public-ip> \
INSTALL_SSL=false \
COORDINATOR_PORT=8080 \
task deploy:frontend
```

**По домену (SSL):**

```bash
DOMAIN=mytonprovider.org \
PUBLIC_ORIGIN=https://mytonprovider.org \
INSTALL_SSL=true \
task deploy:frontend
```

Откройте в firewall `80` / `443` при необходимости.

## Firewall (пример ufw)

```bash
ufw allow OpenSSH
ufw allow 8080/tcp    # coordinator API (или только через nginx :80)
ufw allow 80/tcp      # frontend
ufw allow 443/tcp     # frontend + SSL
ufw allow 16167/udp   # coordinator ADNL
# Postgres 5432 наружу не открывать
ufw enable
```

Prometheus/Grafana (`9092`, `3001`) — только если нужны снаружи.
