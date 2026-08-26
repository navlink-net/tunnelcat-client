# ShortNerdCat — План тестирования

Каждая итерация имеет свой набор тестов. Тесты запускаются автоматически (CI / вручную командой).

---

## Итерация 1 — Серверный core

### T-S1 · db.py

```bash
python -m pytest apps/tunnel/tests/test_db.py -v
```

| Тест | Проверяет |
|---|---|
| `test_init_creates_tables` | После `init_db()` таблицы `users`, `tunnel_sessions`, `traffic` существуют |
| `test_flush_traffic_insert` | `flush_traffic({"alice": {"up":100,"down":200}})` → строка в `traffic` |
| `test_flush_traffic_accumulate` | Второй `flush_traffic` с тем же user/час → суммирует, не дублирует |

### T-S2 · traffic.py

```bash
python -m pytest apps/tunnel/tests/test_traffic.py -v
```

| Тест | Проверяет |
|---|---|
| `test_record_increments` | `record("alice", 100, 200)` дважды → in-memory `{up:200, down:400}` |
| `test_flush_clears_counters` | После flush счётчики обнуляются |
| `test_background_flush` | Через 65 с данные появляются в SQLite (интеграционный, `pytest.mark.slow`) |

### T-S3 · sessions.py

```bash
python -m pytest apps/tunnel/tests/test_sessions.py -v
```

| Тест | Проверяет |
|---|---|
| `test_get_or_create_connects` | `get_or_create(id, "httpbin.org", 80)` → возвращает socket, соединение живое |
| `test_reuse_same_id` | Повторный вызов с тем же `conn_id` → тот же socket |
| `test_watchdog_closes_stale` | После `SESSION_TIMEOUT` секунд socket закрыт и удалён из пула |
| `test_close_explicit` | `close(conn_id)` → socket закрыт немедленно |

### T-S4 · main.py — HTTP-эндпоинт

Запускается тестовый экземпляр Camerlengo `--tunnel` на порту 19443.  
Тестовый пользователь `testuser` / `testpass` создаётся фикстурой.

```bash
python -m pytest apps/tunnel/tests/test_handler.py -v
```

| Тест | Проверяет |
|---|---|
| `test_unauth_returns_404` | POST `/api/media/upload` без `X-Session` → 404 |
| `test_bad_session_returns_404` | POST с невалидным `X-Session` → 404 |
| `test_rate_limit_bans_after_5_failures` | 6 неудачных авторизаций с одного IP → 6-й запрос возвращает 404 без проверки credentials |
| `test_connect_to_target` | Валидный POST с `X-Seq: 0`, `X-Target: base64("httpbin.org:80")` → 200, поле `data` в JSON |
| `test_data_roundtrip` | Отправить HTTP GET внутри туннеля к `httpbin.org/get` → ответ содержит `"url"` |
| `test_upload_multipart_format` | Content-Type запроса `multipart/form-data`, part объявлен как `image/jpeg` |
| `test_response_json_format` | Ответ — валидный JSON `{"status":"ok","id":"...","data":"..."}` |
| `test_traffic_recorded` | После roundtrip в SQLite есть запись с `bytes_up > 0` и `bytes_down > 0` |
| `test_padding_min_512` | Ответ с пустыми данными от target → `len(base64decode(data)) >= 512` |

### T-S5 · discovery.py

```bash
python -m pytest apps/tunnel/tests/test_discovery.py -v
```

| Тест | Проверяет |
|---|---|
| `test_keypair_generated_on_first_run` | Файл ключа создаётся при первом вызове |
| `test_keypair_stable` | Повторный вызов → тот же публичный ключ |
| `test_signed_list_verifiable` | Результат `get_signed_list()` проходит Ed25519 verify |
| `test_tampered_list_fails_verify` | Изменение `servers` без пересчёта подписи → verify падает |

---

## Итерация 2 — Клиентский core (без TUN)

```bash
cd d:/REPO/shortnerdcat && go test ./core/... -v
```

### T-C1 · key.go

| Тест | Проверяет |
|---|---|
| `TestKeyDecode_Valid` | Корректная key-string → `KeyData` с правильными полями |
| `TestKeyDecode_WrongMagic` | Неправильные magic bytes → ошибка |
| `TestKeyDecode_Tampered` | Изменён 1 байт шифртекста → ошибка аутентификации |
| `TestKeyEncode_Roundtrip` | Encode → Decode → исходные данные |

### T-C2 · headers.go

| Тест | Проверяет |
|---|---|
| `TestHeadersConsistentPerSession` | Один session → один и тот же UA во всех вызовах `Headers()` |
| `TestHeadersVaryBetweenSessions` | Два разных session → разные UA (статистически, 10 попыток) |
| `TestHeadersContainRequired` | Каждый набор содержит `User-Agent`, `Accept`, `Accept-Encoding` |

### T-C3 · tunnel.go

Тест поднимает реальный сервер (Camerlengo --tunnel) локально на 127.0.0.1:19443.

| Тест | Проверяет |
|---|---|
| `TestLoginOK` | `Login(url, user, pass)` → непустой session token |
| `TestLoginWrongPassword` | Неверный пароль → ошибка |
| `TestSendReceive_HTTP` | Туннель к `httpbin.org:80`, GET / → 200 |
| `TestChunkSize` | Все отправляемые чанки ≥ 512 байт (padding проверяется) |
| `TestMultipleConns` | 10 параллельных соединений → все получают корректные ответы |

### T-C4 · session.go

| Тест | Проверяет |
|---|---|
| `TestSeqIncrement` | Последовательные Write → X-Seq 0, 1, 2... |
| `TestCloseFlush` | Close → финальный POST уходит на сервер |

### T-C5 · socks5.go (интеграционный)

```bash
# Требует запущенного сервера
curl --socks5 127.0.0.1:1080 http://httpbin.org/get
```

| Тест | Проверяет |
|---|---|
| `TestSOCKS5_Connect` | CONNECT к httpbin.org:80 → 200 |
| `TestSOCKS5_Parallel` | 20 параллельных CONNECT → все завершаются без зависания |
| `TestSOCKS5_UDPAssociate` | UDP ASSOCIATE → корректный отказ (не panic) |

---

## Итерация 3 — TUN + Windows

Тесты на этом уровне требуют Windows + права администратора + установленный WinTun.

```powershell
go test ./windows/... -v   # запускать от Admin
```

| Тест | Проверяет |
|---|---|
| `TestTUNCreate` | WinTun-интерфейс создаётся и уничтожается без ошибок |
| `TestRoutesAdd` | Default route через TUN добавляется |
| `TestRoutesRestore` | После `Stop()` исходный default gateway восстановлен |
| `TestFullStack_Ping` | Ping 8.8.8.8 через TUN → ответ приходит |
| `TestDNS_NoLeak` | DNS-запросы идут через туннель (tcpdump: нет UDP 53 вне TUN) |

### Smoke-тест для Windows (ручной чеклист)

После сборки `.exe` и установки:

1. Запустить от администратора
2. Ввести валидный key-string
3. Убедиться что иконка трея — Connected
4. Открыть `https://api.ipify.org` в браузере → IP совпадает с западным сервером
5. Отключиться → IP вернулся к локальному
6. Принудительно убить процесс → маршруты восстановлены (проверить `route print`)

---

## Итерация 4 — Discovery

```bash
go test ./core/ -run Discovery -v
```

| Тест | Проверяет |
|---|---|
| `TestDiscovery_FetchAndVerify` | GET `/api/nodes/list` → список проходит Ed25519 verify |
| `TestDiscovery_TamperedSig` | Подменённая подпись → ошибка, список не применяется |
| `TestDiscovery_Persistence` | Список сохраняется на диск, загружается при следующем запуске |
| `TestDiscovery_FallbackOffline` | Сервер недоступен → используется кэшированный список |

---

---

## Итерация — Транспортное усиление (DPI-устойчивость)

```bash
go test ./core/ -run "TLS|Pacing|H2|Decoy" -v
```

### T-CT1 · uTLS

| Тест | Проверяет |
|---|---|
| `TestTLS_FingerprintIsChrome` | ClientHello клиента совпадает с Chrome по cipher suites, extensions, порядку |
| `TestTLS_SessionTicketReused` | Повторное соединение использует session ticket (0-RTT поведение) |
| `TestTLS_ALPNContainsH2` | ALPN в ClientHello содержит `h2` |
| `TestTLS_DifferentPresetEachSession` | Два запуска клиента → разные minor-версии Chrome preset |

> Инструмент верификации: `tlsfingerprint.io` или локальный перехват через `mitmproxy --rawtls`

### T-CT2 · Pacing

| Тест | Проверяет |
|---|---|
| `TestPacing_RateLimit` | При burst 20 запросов подряд → реально уходит не быстрее N/с |
| `TestPacing_ChunkSizeDistribution` | 1000 чанков → медиана ~8 КБ, нет равномерного распределения |
| `TestPacing_BackpressureBlocks` | SOCKS5 Write блокируется если token bucket пуст |
| `TestPacing_JitterNonZero` | Задержки между запросами не равномерны (stddev > 5 мс) |

### T-CT3 · HTTP/2

| Тест | Проверяет |
|---|---|
| `TestH2_NegotiatedProtocol` | После handshake `ConnectionState().NegotiatedProtocol == "h2"` |
| `TestH2_ParallelStreams` | 5 параллельных туннельных соединений → одно TCP-соединение к серверу |
| `TestH2_Throughput` | H2-транспорт не медленнее HTTP/1.1 на одном потоке (regression) |
| `TestH2_ServerSupport` | Camerlengo отвечает по h2 при ALPN `h2` |

### T-CT4 · Decoy

| Тест | Проверяет |
|---|---|
| `TestDecoy_FiresInBackground` | При активной сессии → decoy GET уходят раз в ~60 с |
| `TestDecoy_SilentWhenIdle` | Без активных сессий → decoy не отправляются |
| `TestDecoy_SameFingerprintAsTunnel` | Decoy-соединение использует тот же uTLS preset |
| `TestDecoy_NoUserData` | Decoy запросы не содержат cookies, auth headers, user-agent с nodeId |

### Сквозная проверка (ручная)

```bash
# Запустить mitmproxy в режиме перехвата TLS
mitmproxy --mode transparent --ssl-insecure

# Проверить через tlsfingerprint.io (или ja3er.com)
# Ожидаем JA3 hash совпадающий с Chrome
```

---

## Phase 2 — Overlay Network

---

## Итерация 5 — Серверная инфраструктура

### T-S7 · Keygen — расширение SNC_Tunnel_Keygen.py

```bash
python reforce/Tools/SNC_Tunnel_Keygen.py alice s3cr3t "host1:443,host2:443" --pubkey <base64>
```

| Тест | Проверяет |
|---|---|
| `test_keygen_with_pubkey` | key-string с `--pubkey` декодируется Go-клиентом, pubkey присутствует |
| `test_keygen_pubkey_roundtrip` | pubkey из key-string совпадает с переданным оператором |
| `test_keygen_no_pubkey_backward_compat` | key-string без `--pubkey` по-прежнему декодируется (Phase 1 совместимость) |

### T-S8 · Proxy Host

```bash
python -m pytest apps/proxy/tests/ -v
```

| Тест | Проверяет |
|---|---|
| `test_exit_healthcheck_alive` | Живой exit-хост → попадает в пул балансировщика |
| `test_exit_healthcheck_dead` | Exit не отвечает → исключается из пула, трафик не идёт |
| `test_lb_weighted_by_rtt` | Два exit с RTT 10ms и 100ms → 90%+ трафика на быстрый |
| `test_client_register` | `POST /p/v1/register` → клиент в реестре |
| `test_peer_list_returns_relays` | `GET /p/v1/peers` → только клиенты с `canRelay=true` |
| `test_gossip_syncs_peers` | Два proxy-хоста обмениваются реестрами → peer виден с обоих |
| `test_signal_ws_connect` | WebSocket `/p/v1/signal` принимает соединение, пингует |

### T-S9 · Exit Host

```bash
python -m pytest apps/exit/tests/ -v
```

| Тест | Проверяет |
|---|---|
| `test_accept_from_trusted_proxy` | Соединение с валидным токеном proxy → принято |
| `test_reject_from_unknown` | Соединение без токена → отклонено |
| `test_traffic_forwarded` | Proxy → exit → echo-сервер → данные возвращаются proxy |
| `test_metrics_reported` | Exit периодически отправляет RTT и нагрузку на proxy |

---

## Итерация 6 — Клиент: bootstrap и регистрация

```bash
go test ./core/ -run "Bootstrap|Registry|Signal" -v
```

| Тест | Проверяет |
|---|---|
| `TestBootstrap_FromKeyString` | Proxy-хосты и pubkey извлекаются из key-string, не из сети |
| `TestBootstrap_CachePersists` | После первого подключения список proxy кешируется на диск |
| `TestBootstrap_CacheFallback` | Все proxy из key-string недоступны → используется кеш предыдущей сессии |
| `TestBootstrap_PeerListVerified` | Peer-list от proxy проходит Ed25519 verify ключом из key-string |
| `TestBootstrap_TamperedPeerList` | Подменённый peer-list → отклонён, старый кеш сохраняется |
| `TestRegistry_RegisterAll` | Регистрация на N proxy-хостах → успех на всех живых |
| `TestRegistry_HeartbeatCycle` | Heartbeat уходит на все proxy раз в 30 с |
| `TestRegistry_PeerListMerged` | Peer lists от разных proxy-хостов объединяются без дублей |
| `TestSignal_Reconnect` | Обрыв WebSocket → переподключение с backoff |
| `TestSignal_ReceivePeerUpdate` | Proxy пушит `peer_list_update` → локальный реестр обновляется |

---

## Итерация 7 — NAT Traversal

```bash
go test ./core/ -run "NAT|HolePunch" -v
```

| Тест | Проверяет |
|---|---|
| `TestNAT_ObservedEndpoint` | STUN-ответ парсится, IP:port сохраняется |
| `TestNAT_TypeDetection` | Тип NAT определяется (full cone / symmetric / etc) |
| `TestHolePunch_SuccessBothSides` | Два клиента пробивают NAT одновременно → UDP-пакеты приходят с обеих сторон |
| `TestHolePunch_Timeout` | Пир не отвечает 3 с → ошибка, не зависает |
| `TestHolePunch_SymmetricSkipped` | NAT type symmetric → попытка не делается, сразу fallback |

### Стендовый тест (требует двух машин за NAT)

```
Машина A (за NAT 1) ←──── Proxy Host (публичный IP) ────► Машина B (за NAT 2)
                    сигнал hole_punch                 сигнал hole_punch
                    A и B одновременно шлют UDP
                    → прямое соединение установлено
```

---

## Итерация 8 — Relay Mesh

```bash
go test ./core/ -run "Relay|Router" -v
```

| Тест | Проверяет |
|---|---|
| `TestRelay_AcceptRequest` | Сигнал `relay_request` → клиент принимает, форвардит трафик |
| `TestRelay_RejectIfOverload` | Превышен лимит соединений → запрос отклонён с `relay_busy` |
| `TestRelay_RejectIfDisabled` | `canRelay=false` → запрос отклонён |
| `TestRelay_TrafficAccounted` | После форварда: `relay_tx_bytes` и `relay_rx_bytes` ненулевые |
| `TestRelay_NATClientViaRendezvous` | NAT-клиент получает `relay_request` через persistent channel → форварди через stationery |
| `TestRouter_DirectFirst` | Если direct доступен → выбирается direct (RTT проверяется) |
| `TestRouter_FallbackToRelay` | Direct недоступен → переключается на relay без потери данных |
| `TestRouter_ScoreByRTT` | Три relay-кандидата → побеждает с наименьшим RTT × hops |
| `TestRouter_PathTTL` | Путь истекает → пересчёт; трафик не прерывается |
| `TestRouter_WarmBackup` | При деградации основного пути → backup активируется без gap |

### Сквозной тест — peer-to-peer через relay (ручной)

```
Client A ──(relay)──► Client B-as-relay ──► Exit Host ──► internet
```

1. Запустить Client B с `canRelay=true`
2. Запустить Client A, убедиться что он видит B в peer list
3. Принудительно отключить direct path (firewall rule)
4. A соединяется через B → `https://api.ipify.org` возвращает IP exit-хоста
5. Убить Client B → A переключается на proxy fallback без разрыва сессии

---

## Итерация 9 — State Machine

```bash
go test ./core/ -run StateMachine -v
```

| Тест | Проверяет |
|---|---|
| `TestSM_HappyPath` | `Idle → Bootstrapping → Registering → Discovering → ConnectingDirect → Connected` |
| `TestSM_RelayFallback` | Direct fail → `ConnectingRelay → Connected` |
| `TestSM_Degraded` | Loss > 20% → `Degraded → Rebuilding → Connected` |
| `TestSM_AuthFail` | Auth fail → `Closed`, повтор через backoff |
| `TestSM_TrayReflectsState` | Каждый переход → tray-иконка и меню обновляются |

---

## Инфраструктура тестирования

### Серверные тесты

Фикстура `conftest.py` в `apps/tunnel/tests/`:
- Запускает временный Camerlengo `--tunnel` на `127.0.0.1:19443`
- Создаёт тестового пользователя `testuser / testpass` через `verifyPassword`
- После тестов убивает процесс, удаляет временную БД

### Go-тесты

Переменная окружения `TUNNEL_TEST_SERVER=http://127.0.0.1:19443` указывает на локальный сервер.  
Если не задана — тесты, требующие сервер, пропускаются (`t.Skip()`).

### Маркировка тестов

| Метка | Смысл |
|---|---|
| (без метки) | Unit-тест, не требует внешних сервисов |
| `pytest.mark.integration` / `t.Short()` skip | Требует локального сервера |
| `pytest.mark.slow` | Ждёт таймеры (watchdog, flush) |
| `pytest.mark.admin` | Требует прав администратора Windows |
