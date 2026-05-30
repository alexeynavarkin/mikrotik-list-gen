# ru-list-server

Небольшой Go-сервис, который скачивает список российских IP-подсетей и отдаёт его
по HTTP в виде RouterOS-скрипта (`.rsc`) для импорта в MikroTik
`address-list`. Используется для split-tunneling: трафик в RU идёт через WAN
провайдера, остальной — в WireGuard-туннель.

Только стандартная библиотека, без внешних зависимостей.

## Источники данных

| Назначение | URL | Формат |
|------------|-----|--------|
| IPv4 (основной) | `https://www.ipdeny.com/ipblocks/data/countries/ru.zone` | CIDR на строку |
| IPv6 (основной) | `https://www.ipdeny.com/ipv6/ipaddresses/blocks/ru.zone` | CIDR на строку |
| Фолбэк (v4+v6)  | `https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest` | реестр RIPE |

Поведение:

- При старте — **синхронная** загрузка. Если не удалось — `log.Fatal` (сервис без
  данных бесполезен).
- Фоновое обновление по таймеру (`-refresh`, дефолт `24h`).
- Если очередное обновление упало — остаётся **предыдущий** успешный список,
  пустота не отдаётся.
- Замена снапшота атомарна (`sync.RWMutex`).
- Каждый CIDR валидируется через `net.ParseCIDR` и приводится к каноническому виду;
  мусор отбрасывается.
- IPv4 обязателен (есть фолбэк на RIPE). **IPv6 — best effort**: если он не
  загрузился, сервис продолжает работать с одним IPv4.
- Для RIPE-фолбэка IPv4 `value` — это количество адресов; диапазон
  `[start, start+value)` раскладывается на минимальный набор выровненных CIDR
  (RIPE-блоки не всегда являются степенью двойки).

## HTTP-эндпоинты

| Эндпоинт | Содержимое |
|----------|------------|
| `GET /ru-list.rsc`    | IPv4 + IPv6 в одном файле |
| `GET /ru-list-v4.rsc` | только IPv4 |
| `GET /ru-list-v6.rsc` | только IPv6 |
| `GET /healthz`        | JSON-статус: источник, кол-во записей, время обновления |

Пример `healthz`:

```json
{
  "status": "ok",
  "source": "ipdeny",
  "entries_v4": 17234,
  "entries_v6": 920,
  "updated_at": "2026-05-30T14:30:00Z",
  "age_seconds": 137
}
```

## Флаги

| Флаг | Дефолт | Описание |
|------|--------|----------|
| `-addr`    | `:8080` | адрес прослушивания |
| `-refresh` | `24h`   | интервал фонового обновления |

## Формат выходного `.rsc`

Файл сначала удаляет ранее сгенерированные записи (по маркеру в комментарии),
затем добавляет актуальные. Фильтр по `comment="ru-auto"` гарантирует, что
ручные записи в том же листе не пострадают.

```routeros
# auto-generated 2026-05-30T14:30:00Z, entries=17234
/ip firewall address-list remove [find list=RU comment="ru-auto"]
/ip firewall address-list add list=RU address=2.56.88.0/22 comment="ru-auto"
/ip firewall address-list add list=RU address=5.8.8.0/21 comment="ru-auto"
...
/ipv6 firewall address-list remove [find list=RU comment="ru-auto"]
/ipv6 firewall address-list add list=RU address=2a00:1118::/29 comment="ru-auto"
...
```

Имя листа — `RU`, комментарий-маркер — `ru-auto`.

> ⚠️ Между `remove` и завершением всех `add` адрес-лист в RouterOS какое-то время
> (≈10–30 с на больших списках) неконсистентен — часть RU-подсетей временно
> отсутствует. Для суточного обновления split-tunnel это приемлемо; если нет —
> импортируйте во временный лист и переключайте правила атомарно.

## Запуск

```sh
go build -o ru-list-server .
./ru-list-server -addr :8080 -refresh 24h
```

### Docker

```sh
docker build -t ru-list-server .
docker run -d --name ru-list -p 8080:8080 --restart unless-stopped ru-list-server
```

Образ — multi-stage на distroless, статический бинарь (`CGO_ENABLED=0`),
запуск от непривилегированного пользователя.

### systemd (на VPS)

`/etc/systemd/system/ru-list-server.service`:

```ini
[Unit]
Description=ru-list-server (RU address-list for MikroTik)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/ru-list-server -addr :8080 -refresh 24h
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
sudo install -m 0755 ru-list-server /usr/local/bin/
sudo systemctl daemon-reload
sudo systemctl enable --now ru-list-server
```

## Потребление на MikroTik

MikroTik не умеет тянуть address-list по URL напрямую (у `address-list` нет
параметра `url` — проверено по официальной документации). Работает только связка
`/tool fetch` + `/import`:

```routeros
/system script add name=update-ru-list source={
    :do {
        /tool fetch url="http://<host>:8080/ru-list.rsc" mode=http dst-path=ru-list.rsc
        /import file-name=ru-list.rsc
    } on-error={ :log error "RU list update failed" }
}
/system scheduler add name=update-ru-daily start-time=04:00:00 interval=1d \
    on-event="/system script run update-ru-list"
```

## Тесты

```sh
go test ./...
```

Покрыты парсер RIPE-дампа (`parseRIPE`), раскладка диапазона на CIDR
(`ipv4RangeToCIDRs`), парсер zone-файлов (`parseZone`) и рендер скрипта
(`renderRSC`).
