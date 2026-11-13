# xray-waybar-ctl

CLI на Go, который управляет `xray-core` и отдаёт статус кастомному модулю Waybar.
Без GUI, без Electron — `xray-waybar-ctl status` печатает одну строку JSON, всё остальное
прячется за `connect` / `disconnect` / `use-next`.

## Возможности

- Скачивает подписку (base64 → список `vless://` / `vmess://` / `trojan://`).
- Парсит VLESS+REALITY, TLS, WS, gRPC, XHTTP (включая имя `splithttp`).
- Параллельно URL-тестит серверы и выбирает живой с минимальной латентностью
  (или первый из заданного `priority`).
- Запускает xray-core как detached процесс с PID-файлом, ждёт открытия SOCKS5 порта.
- Печатает JSON для Waybar (`connected` / `disconnected` / `loading` / `error`).
- Команды переключения: `use <name>`, `use-next`, `use-prev`, `reconnect`, `toggle`.

## Установка

Нужно поставить:

- `xray-core` — основной демон
- `waybar` — куда выводится статус
- `walker` — dmenu-лаунчер для встроенного меню переключения серверов
  (`xray-waybar-ctl menu`)
- Go ≥ 1.21 — для сборки
- `tun2socks` — только если планируется `system_wide: true`

На Arch это всё в `pacman`/AUR (`yay -S xray waybar walker-bin tun2socks go`).
На Debian/Ubuntu/Fedora — штатный пакетный менеджер; `walker` и `tun2socks`
если в репах нет, берутся из релизов:
<https://github.com/abenz1267/walker>,
<https://github.com/xjasonlyu/tun2socks/releases>.

```sh
make install
```

`make install` кладёт бинарь в `~/.local/bin/xray-waybar-ctl` и пример конфига
в `~/.local/share/xray-waybar/app.yaml.example`. Убедись, что `~/.local/bin`
есть в `$PATH`.

### Что предполагает проект

- **systemd как init** — мониторинг и TUN-сайдкар поднимаются через
  systemd-юниты (user-session и system). Без systemd `make install-monitor`
  и `make install-system` не сработают; основной `connect/disconnect`
  будет работать, но без автореконнекта и без TUN.
- **logind на system D-Bus** — watchdog ловит `PrepareForSleep` от
  `org.freedesktop.login1` для tear-down/resume. На системах без logind
  (runit/OpenRC) suspend/resume в watchdog'е молча отключится, тики по
  таймеру остаются.
- **polkit + группа `wheel`** — нужны только для `system_wide: true`,
  чтобы юзер мог поднимать `xray-waybar-tun.service` без пароля
  (`configs/polkit/50-xray-waybar.rules`). На дистрибутивах, где
  административная группа называется иначе (`sudo` в Debian),
  отредактируй правило либо добавь себя в `wheel`:
  `sudo groupadd -f wheel && sudo usermod -aG wheel "$USER"`.
- **nerd-font на панели** — иконки в `internal/waybar` это
  нерд-глифы. Если у тебя нет нерд-шрифта, в Waybar будут квадратики
  — поставь любой nerd-font и пропиши его в `font-family` стиля
  модуля.
- **pacman-хук** (`configs/pacman/xray-waybar.hook`) реприменяет
  `cap_net_admin` на `/usr/bin/xray` после `pacman -Syu`. На не-Arch
  дистрибутивах хук не сработает — после обновления xray руками:
  `sudo setcap cap_net_admin+ep /usr/bin/xray`.

## Конфигурация

Скопируй пример и подставь свой URL подписки:

```sh
mkdir -p ~/.config/xray-waybar
cp ~/.local/share/xray-waybar/app.yaml.example ~/.config/xray-waybar/app.yaml
$EDITOR ~/.config/xray-waybar/app.yaml
```

Минимально нужен только `subscription_url`. Все остальные поля имеют разумные дефолты —
см. `configs/app.yaml.example`.

Если `routing_profile` не `proxy-all` / `direct` — один раз нужно скачать
geoip/geosite, иначе xray откажется стартовать:

```sh
xray-waybar-ctl update-geo
```

Дальше это делает таймер `xray-waybar-geo.timer` из `make install-monitor`.

## Использование

```sh
xray-waybar-ctl update      # подтянуть подписку в локальный кэш
xray-waybar-ctl list        # посмотреть, что в кэше
xray-waybar-ctl test        # таблица латентностей
xray-waybar-ctl connect     # тест + автоматический выбор + запуск
xray-waybar-ctl use DE-2    # подключиться к конкретному серверу
xray-waybar-ctl status      # JSON для Waybar
```

### XHTTP

В ссылках `vless://` / `trojan://` поддерживаются `type=xhttp`, `path`, `host`,
`mode` и `extra` (JSON-объект, закодированный как URL-параметр).
В JSON-ссылках `vmess://` — `net: "xhttp"` и те же поля; `extra` может быть
объектом или строкой с JSON. Параметры `extra`, включая `xmux` и
`downloadSettings`, сохраняются в кэше и передаются xray-core без изменений.
Без `path` используется `/`, без `mode` — `auto`. TLS и REALITY настраиваются
обычными параметрами ссылки. В Clash YAML из `xhttp-opts` читаются `path`,
`host`, `mode` и `headers`.

Пример ссылки (UUID и адрес нужно заменить своими):

```text
vless://00000000-0000-0000-0000-000000000001@vpn.example.com:443?encryption=none&security=tls&sni=vpn.example.com&type=xhttp&path=%2Fvpn&mode=auto#XHTTP
```

После обновления клиента пересобери его и обнови подписку, чтобы новые поля
попали в кэш:

```sh
make install
xray-waybar-ctl update
xray-waybar-ctl reconnect
```

Установленный `xray-core` также должен поддерживать XHTTP:
[описание транспорта в Project X](https://xtls.github.io/en/config/transports/xhttp.html).

## System-wide режим (весь трафик через VPN)

По умолчанию xray-waybar-ctl поднимает только локальный SOCKS5 на `127.0.0.1:1080`,
и приложения должны сами через него ходить. Чтобы весь трафик системы автоматически
шёл через VPN — включи system-wide режим через TUN.

### Установка

```sh
yay -S tun2socks            # gVisor-based, лежит в AUR
sudo make install-system    # systemd unit + polkit rule + pacman hook
```

`install-system` ставит:
- `xray-waybar-tun.service` (systemd unit для tun2socks)
- polkit-правило, разрешающее членам группы `wheel` поднимать/гасить именно этот unit без пароля
- pacman-хук, который реприменяет `cap_net_admin` на `/usr/bin/xray` после каждого `pacman -Syu`

Пользователь должен быть в группе `wheel` — иначе polkit откажет и `connect`
не сможет поднять TUN:

```sh
groups | grep -q wheel || sudo usermod -aG wheel "$USER"   # перелогиниться
```

В `~/.config/xray-waybar/app.yaml`:

```yaml
system_wide: true
```

После этого `xray-waybar-ctl connect` запускает xray и поднимает `xray-waybar-tun.service`
— весь IP-трафик уходит в `tun0`, кроме приватных подсетей:

- `10.0.0.0/8`
- `172.16.0.0/12`
- `192.168.0.0/16`

Они маршрутизируются мимо туннеля, как и был запрос. Список захардкожен в
`configs/systemd/xray-waybar-tun.service` — если нужны ещё bypass-CIDR, добавляй
`ExecStartPost`/`ExecStopPost` строки там и `sudo systemctl daemon-reload`.

`disconnect` останавливает unit перед тем как глушить xray (иначе пакеты ушли бы
в мёртвый SOCKS5). Если хочешь убрать system-wide вообще — `sudo make uninstall-system`.

Проверка:
```sh
ip route                                # должен быть `default dev tun0 metric 1`
curl https://ifconfig.me                # IP сервера, не твой
curl http://10.0.0.1                    # идёт напрямую (если этот хост у тебя есть)
```

## Мониторинг и автореконнект

```sh
make install-monitor
```

Ставит и включает три user-юнита:

- `xray-waybar-ping.timer` — раз в 30s гоняет ping-батч, чтобы тултип и
  меню walker показывали актуальные латентности.
- `xray-waybar-watchdog.service` — long-lived демон с внутренним 10s
  loop. Ловит две вещи: (1) xray умер за tun2socks → автореконнект,
  (2) система проснулась после suspend → tear down + reconnect через
  D-Bus signal `PrepareForSleep` от logind.
- `xray-waybar-geo.timer` — раз в сутки обновляет `geoip.dat` /
  `geosite.dat` из `roscomvpn-geo*` GitHub Releases.

`test` проверяет HTTP-запрос через прокси, а фоновый `ping` — только
доступность TCP-порта. Успешный TCP-пинг не отменяет ошибку последнего
URL-теста: сервер с открытым портом может не пропускать трафик через
TLS/REALITY/XHTTP. Результаты сохраняются отдельно в `url_test` и `tcp_ping`
в `state.json`; поля `alive`, `latency_ns` и `error` отражают итоговый статус
для панели. До первого URL-теста отображается TCP-результат. В старом
`state.json` источник проверки неизвестен — запусти `xray-waybar-ctl test`
после обновления, чтобы получить полноценные результаты.

Снять: `make uninstall-monitor`.

Логи watchdog'а:

```sh
systemctl --user status xray-waybar-watchdog.service
journalctl --user -u xray-waybar-watchdog.service -f
```

## Меню переключения

Команда `xray-waybar-ctl menu` поднимает `walker --dmenu` со списком
серверов; первой строкой — текущий профиль. Выбор сервера → `use`,
выбор `Profile: …` → второй walker с вариантами `routing_profile`
(proxy-all / direct / smart / whitelist / custom URL). Запись идёт в
`state.json` и перебивает значение из `app.yaml` до
`xray-waybar-ctl profile reset`.

Биндить на хоткей оконного менеджера или на `on-click` модуля Waybar.
`walker` должен быть в `$PATH` — иначе `menu` вернёт ошибку, остальной
CLI работает.

## Waybar

`~/.config/waybar/config.jsonc`:

```jsonc
"custom/vpn": {
    "exec": "xray-waybar-ctl status",
    "interval": 5,
    "return-type": "json",
    "on-click":        "xray-waybar-ctl toggle",
    "on-click-right":  "xray-waybar-ctl reconnect",
    "on-scroll-up":    "xray-waybar-ctl use-next",
    "on-scroll-down": "xray-waybar-ctl use-prev"
}
```

`~/.config/waybar/style.css`:

```css
#custom-vpn               { color: @disabled-color; padding: 0 8px; }
#custom-vpn.connected     { color: @green; }
#custom-vpn.loading       { color: @yellow; }
#custom-vpn.error         { color: @red; }
#custom-vpn.degraded      { color: @orange; }  /* system_wide=true но TUN не активен */
```

После правок: `pkill -SIGUSR2 waybar`.

## Разработка

```sh
make test       # юнит-тесты
make vet
make build      # выдаёт ./xray-waybar-ctl
```

Проверка реального HTTP-запроса через локальные XHTTP и XHTTP+REALITY
туннели (нужен `xray` в `$PATH`, внешняя сеть не используется):

```sh
XRAY_INTEGRATION=1 go test ./internal/tester -run TestXHTTPTunnel -v
```

## Лицензия

MIT.
