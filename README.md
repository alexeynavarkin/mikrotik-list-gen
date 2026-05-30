# geoip-list-server

Небольшой Go-сервис, который скачивает списки IP-подсетей **по странам** (ISO
3166-1 alpha-2) и отдаёт их по HTTP в виде RouterOS-скриптов (`.rsc`) для импорта
в MikroTik `address-list`. Типичный сценарий — split-tunneling: трафик в выбранные
страны идёт через WAN провайдера, остальной — в WireGuard-туннель.

Какие страны обслуживать, задаётся флагом `-countries` (например `ru,by,kz`). Имя
адрес-листа и маркер-комментарий выводятся из кода страны, так что несколько стран
можно импортировать в разные листы, не мешая друг другу.

Только стандартная библиотека, без внешних зависимостей.

## Источники данных

`%s` — код страны в нижнем регистре.

| Назначение | URL | Формат |
|------------|-----|--------|
| IPv4 (основной) | `https://www.ipdeny.com/ipblocks/data/countries/%s.zone` | CIDR на строку |
| IPv6 (основной) | `https://www.ipdeny.com/ipv6/ipaddresses/blocks/%s.zone` | CIDR на строку |
| Фолбэк (v4+v6)  | `https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest` | реестр RIPE |

> ⚠️ Фолбэк RIPE покрывает только регион RIPE NCC (Европа, Ближний Восток,
> Центральная Азия). Для стран других регионов (US, CN, …) фолбэк ничего не даёт —
> используется только ipdeny, который агрегирует все RIR и работает для любой
> страны.

Поведение:

- При старте — **синхронная** загрузка всех заданных стран. Если не загрузилась
  **ни одна** — `log.Fatal`. Если упала отдельная страна — предупреждение, сервис
  стартует с остальными.
- Фоновое обновление по таймеру (`-refresh`, дефолт `24h`).
- Если очередное обновление страны упало — остаётся её **предыдущий** успешный
  список, пустота не отдаётся.
- Снимки по странам меняются атомарно (`sync.RWMutex`); страны строятся параллельно.
- Каждый CIDR валидируется через `net.ParseCIDR` и приводится к каноническому виду;
  мусор отбрасывается.
- IPv4 обязателен (есть фолбэк на RIPE). **IPv6 — best effort**.
- Для RIPE-фолбэка IPv4 `value` — это количество адресов; диапазон
  `[start, start+value)` раскладывается на минимальный набор выровненных CIDR
  (RIPE-блоки не всегда являются степенью двойки).

## HTTP-эндпоинты

`{cc}` — код обслуживаемой страны в нижнем регистре.

| Эндпоинт | Содержимое |
|----------|------------|
| `GET /{cc}.rsc`    | IPv4 + IPv6 в одном файле |
| `GET /{cc}-v4.rsc` | только IPv4 |
| `GET /{cc}-v6.rsc` | только IPv6 |
| `GET /healthz`     | JSON-статус по всем странам |

Запрос страны, которая не обслуживается этим инстансом, отдаёт `404`. Пример при
`-countries ru,by`:

- `GET /ru.rsc` → список RU
- `GET /by-v4.rsc` → только IPv4 BY
- `GET /us.rsc` → `404`

Пример `healthz`:

```json
{
  "status": "ok",
  "countries": {
    "ru": {"source": "ipdeny", "entries_v4": 11344, "entries_v6": 2200, "updated_at": "2026-05-30T14:30:00Z", "age_seconds": 137},
    "by": {"source": "ipdeny", "entries_v4": 920,   "entries_v6": 60,   "updated_at": "2026-05-30T14:30:00Z", "age_seconds": 137}
  }
}
```

`status`: `ok` (все готовы), `degraded` (часть готова), `starting` (ни одной — `503`).

## Флаги

| Флаг | Дефолт | Описание |
|------|--------|----------|
| `-addr`      | `:8080` | адрес прослушивания |
| `-refresh`   | `24h`   | интервал фонового обновления |
| `-countries` | `ru`    | список кодов стран через запятую (`ru,by,kz`) |
| `-healthcheck` | `false` | опросить локальный `/healthz` и выйти (для контейнерного HEALTHCHECK) |

## Формат выходного `.rsc`

Файл сначала удаляет ранее сгенерированные записи **этой страны** (по маркеру в
комментарии), затем добавляет актуальные. Имя листа — код страны в верхнем
регистре, маркер-комментарий — `<cc>-auto`. Фильтр по комментарию гарантирует, что
ручные записи в том же листе не пострадают.

```routeros
# auto-generated 2026-05-30T14:30:00Z, country=RU, entries=11344
/ip firewall address-list remove [find list=RU comment="ru-auto"]
/ip firewall address-list add list=RU address=2.56.88.0/22 comment="ru-auto"
/ip firewall address-list add list=RU address=5.8.8.0/21 comment="ru-auto"
...
/ipv6 firewall address-list remove [find list=RU comment="ru-auto"]
/ipv6 firewall address-list add list=RU address=2a00:1118::/29 comment="ru-auto"
...
```

Имя листа и маркер выводятся из кода страны (см. раздел про MikroTik), поэтому
несколько стран импортируются в разные листы независимо.

## Запуск

```sh
go build -o geoip-list-server .
./geoip-list-server -addr :8080 -refresh 24h -countries ru,by,kz
```

### Docker

```sh
docker build -t geoip-list-server .
docker run -d --name geoip-list -p 8080:8080 --restart unless-stopped \
  geoip-list-server -countries ru,by,kz
```

Образ — multi-stage на distroless, статический бинарь (`CGO_ENABLED=0`),
запуск от непривилегированного пользователя. В образ встроен `HEALTHCHECK` —
бинарь сам опрашивает свой `/healthz` (`-healthcheck`), т.к. в distroless нет
shell/curl.

### Docker Compose

Самый быстрый старт:

```sh
docker compose up -d          # тянет образ из ghcr.io
docker compose up -d --build  # или собрать локально
docker compose logs -f
```

`docker-compose.yml` поднимает сервис на `:8080`, с `restart: unless-stopped` и
healthcheck-ом. Список стран — в `command` (`-countries ...`).

### systemd (на VPS)

`/etc/systemd/system/geoip-list-server.service`:

```ini
[Unit]
Description=geoip-list-server (per-country address-lists for MikroTik)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/geoip-list-server -addr :8080 -refresh 24h -countries ru,by,kz
Restart=on-failure
RestartSec=10
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

```sh
sudo install -m 0755 geoip-list-server /usr/local/bin/
sudo systemctl daemon-reload
sudo systemctl enable --now geoip-list-server
```

## Настройка на MikroTik (RouterOS 7)

MikroTik не умеет тянуть address-list по URL напрямую (у `/ip firewall
address-list` нет параметра `url`). Рабочая связка — `/tool fetch` (скачать файл
в файловую систему роутера) + `/import` (выполнить его как скрипт). Команды ниже
вводятся в терминале (WinBox → **New Terminal**, либо SSH). Замените
`SERVER:8080` на адрес вашего сервиса.

### Параметры команд (по [официальной спеке](https://help.mikrotik.com/docs/spaces/ROS/pages/8978514/Fetch))

- `/tool fetch` — `url=`, `mode=` (`http`|`https`|`ftp`|`sftp`|`tftp`),
  `dst-path=` (куда сохранить в ФС роутера), `output=file` (явно «писать в файл»),
  `check-certificate=` (`no` по умолчанию, для HTTPS — `yes`).
- `/import file-name=` — выполняет `.rsc` как набор консольных команд. Можно
  добавить `verbose=yes` для остановки на первой ошибке (удобно при отладке).

### Вариант 1. Одна страна (RU), по HTTP

```routeros
/system script
add name="geoip-ru-update" policy=read,write,ftp,test source={
    :do {
        /tool fetch url="http://SERVER:8080/ru.rsc" mode=http dst-path="ru.rsc" output=file;
        /import file-name="ru.rsc";
        /file remove [find name="ru.rsc"];
    } on-error={
        :log error "geoip-ru-update: fetch/import failed";
    }
}

/system scheduler
add name="geoip-ru-daily" interval=1d start-time=04:00:00 \
    policy=read,write,ftp,test on-event="/system script run geoip-ru-update"
```

`policy=read,write,ftp,test` нужна и скрипту, и шедулеру: `read`/`ftp` — для
`fetch`, `write`/`test` — чтобы `import` мог менять конфигурацию. Без этого
шедулер молча не применит изменения.

### Вариант 2. Несколько стран одним скриптом

Имена файлов и листов выводятся из кода страны (`/ru.rsc` → лист `RU`,
`/by.rsc` → `BY`, …), поэтому импорт одной страны не задевает другие:

```routeros
/system script
add name="geoip-update" policy=read,write,ftp,test source={
    :local host "SERVER:8080";
    :foreach cc in={"ru";"by";"kz"} do={
        :do {
            /tool fetch url=("http://" . $host . "/" . $cc . ".rsc") \
                mode=http dst-path=($cc . ".rsc") output=file;
            /import file-name=($cc . ".rsc");
            /file remove [find name=($cc . ".rsc")];
        } on-error={
            :log error ("geoip-update: " . $cc . " failed");
        }
    }
}

/system scheduler
add name="geoip-daily" interval=1d start-time=04:00:00 \
    policy=read,write,ftp,test on-event="/system script run geoip-update"
```

### Вариант 3. HTTPS

Если сервис за TLS, используйте `mode=https`. С `check-certificate=yes` корневой
CA должен быть в хранилище роутера (`/certificate`); для Let's Encrypt при
актуальной прошивке он обычно уже есть. Если проверку отключить —
`check-certificate=no` (трафик шифруется, но MITM не отлавливается):

```routeros
/tool fetch url="https://SERVER/ru.rsc" mode=https check-certificate=yes \
    dst-path="ru.rsc" output=file;
```

### Проверка

Запустите вручную и убедитесь, что записи появились:

```routeros
/system script run geoip-ru-update
/ip firewall address-list print count-only where list=RU
/ipv6 firewall address-list print count-only where list=RU
```

> ⚠️ Между `remove` и завершением всех `add` внутри одного `import` лист какое-то
> время неконсистентен (≈10–30 с на больших списках) — часть подсетей временно
> отсутствует. Для суточного обновления split-tunnel это приемлемо.

## Тесты

```sh
go test ./...
```

Покрыты парсер RIPE-дампа (`parseRIPE`), раскладка диапазона на CIDR
(`ipv4RangeToCIDRs`), парсер zone-файлов (`parseZone`), рендер скрипта
(`renderRSC`) и разбор/валидация кодов стран (`parseCountries`, `validCC`).
