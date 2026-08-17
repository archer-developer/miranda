# Универсальный слой OAuth2-авторизации

> Статус: реализовано (`../../internal/oauth2`, `../../internal/mcp`, `../../internal/httpapi`,
> `../../internal/config`, `../../internal/keyring`, `../../internal/httpx`, `../../cmd/miranda`).
> Первый потребитель — Google Calendar, но слой не завязан на Google:
> подключение нового OAuth2-провайдера — это одна запись `oauth.providers` в
> конфиге, без нового Go-кода для самого слоя авторизации (потребитель
> токена — MCP-сервер или нативный тул — своего кода по-прежнему требует).

> **Обновление после реализации:** секции 4-5 ниже (`oauth_authorize`,
> `MCPServerExtension.OAuthProvider`) описывают то, как этот слой
> задумывался и до сих пор устроен для MCP-серверов в целом — механизм
> рабочий и используется. Но для Google Calendar конкретно выяснилось, что
> официальный хостящийся Calendar MCP-сервер
> (`calendarmcp.googleapis.com`, из
> https://developers.google.com/workspace/calendar/api/guides/configure-mcp-server)
> требует отдельного enrollment в Google Workspace Developer Preview
> Program — обычный `enabled: true` в Cloud Console этого не даёт, и
> реальный вызов тула стабильно возвращал `403 The caller does not have
> permission`, при том что тот же самый access-токен тем же самым
> заголовком `Authorization: Bearer` прекрасно работал против обычного,
> общедоступного Calendar API v3 REST (`www.googleapis.com/calendar/v3`,
> проверено напрямую curl'ом). Поэтому Google Calendar в итоге подключён
> **не** как `mcp.servers[]`-запись, а как нативные тулы Miranda
> (`internal/calendar`, `internal/httpapi/calendar_tools.go`,
> `calendar_list_calendars`/`calendar_list_events`/`calendar_freebusy`/
> `calendar_create_event`/`calendar_update_event`/`calendar_delete_event`) —
> `oauth.providers.google_calendar` (авторизация, хранение, refresh токена)
> используется как есть, без изменений, просто "потребитель" токена теперь
> не MCP-сессия, а прямой HTTP-вызов Calendar API. См. `internal/calendar`'s
> собственный doc-comment.

---

## 1. Проблема

Единственный существовавший механизм авторизации MCP-серверов —
статический bearer-токен на весь сервер (`config.MCPServer.TokenEnv` →
`os.Getenv` → захватывается один раз в `bearerTransport` при
`mcp.Connect`, см. `../../internal/mcp/sdkclient.go:28-31`). Для Google
Calendar это не подходит принципиально: у каждого члена семьи свой Google-
аккаунт, доступ выдаётся через OAuth 2.1 с явным согласием пользователя, и
токен нужно уметь обновлять (`refresh_token`) без участия человека.

Требовалось: (1) универсальный слой хранения/обновления OAuth2-токенов,
переиспользуемый для будущих провайдеров без переписывания кода; (2)
удобный сценарий первого входа — отдельный tool, который отправляет
пользователю ссылку на авторизацию, и вебхук, который эту авторизацию
завершает.

---

## 2. Два решения, принятых до реализации

### 2.1 Отдельная MCP-сессия на каждого пользователя

MCP Streamable HTTP делает `initialize` один раз на сессию; сервер (тем
более официальный Google Calendar MCP, следующий MCP Authorization spec,
OAuth 2.1) с высокой вероятностью привязывает идентичность к этому
конкретному handshake, а не проверяет Bearer-заголовок заново на каждый
вызов инструмента. Подменять токен в уже установленной сессии небезопасно:
в лучшем случае сервер проигнорирует новый токен, в худшем — начнёт путать
пользователей (один увидит календарь другого).

Поэтому `mcp.Manager` получил второе измерение ключа: `clientKey{server,
userID}` (`../../internal/mcp/mcp.go`). Для серверов, не отмеченных как
OAuth-gated (`SetOAuthServers`), `userID` в ключе всегда пусто — это
старое, никак не изменившееся поведение с одним общим соединением на
сервер. Для OAuth-gated сервера каждый пользователь получает свою сессию,
поднимаемую лениво (`EnsureUserSession`) и живущую под собственным
`keepConnectedKeyed`-циклом (тот же backoff/health-check, что и у
глобального `KeepConnected`). В конфиге при этом по-прежнему один блок
`mcp.servers` на `google_calendar` — мультиплексирование по пользователям
целиком внутри `Manager`, невидимо снаружи.

### 2.2 Шифрование refresh-токенов ключом из `*_env`, а не через `internal/keyring`

`internal/keyring` шифрует данные пользовательским мастер-ключом,
доступным только когда keyring разблокирован в памяти (passkey/пароль).
Если бы refresh-токены шифровались этим ключом, фоновый refresh и
scheduled-задачи переставали бы работать всякий раз, когда пользователь не
залогинен ни в одном канале — календарь мог бы "отваливаться" в фоне без
видимой причины.

Вместо этого `internal/oauth2` шифрует токены AES-256-GCM-ключом,
загружаемым из отдельной переменной окружения (`OAuthConfig.MasterKeyEnv`,
`internal/oauth2/masterkey.go`) — тот же `*_env`-паттерн, что уже
используют `TokenEnv`/`APIKeyEnv`. Криптопримитивы переиспользованы из
`internal/keyring/crypto.go` (`Wrap`/`Unwrap` экспортированы отдельным
коммитом специально для этого), а не продублированы. Модель угрозы та же,
что уже принята для остальных `*_env`-секретов: это защищает от утечки
файла БД/бэкапа отдельно от окружения процесса, но не от полной
компрометации хоста — то же самое ограничение, что и у любого другого
`*_env`.

---

## 3. Структура `internal/oauth2`

- `provider.go` — `Provider`: конфиг одного OAuth2-провайдера (URL'ы,
  client id/secret, scopes, PKCE, extra authorize params).
- `pkce.go` — `GenerateVerifier`/`ChallengeS256` (RFC 7636).
- `state.go` — `PendingAuthStore`: TTL-хранилище незавершённых попыток
  авторизации, ключ — одноразовый `state` (генерируется как
  `attachments.NewFileID`/`telegram.RandomSecret` — 24 случайных байта в
  hex). Используется вместо сессионной cookie, потому что пользователь
  может открыть ссылку авторизации из другого канала (Telegram на
  телефоне), а не из аутентифицированной вкладки браузера.
- `store.go` — `Store`: SQLite (`config.StorageConfig.OAuthSQLitePath`,
  по умолчанию `./data/oauth.db`), таблица `oauth_tokens(username,
  provider, access_token_enc, refresh_token_enc, ...)`, upsert по
  `(username, provider)` — паттерн один в один с
  `internal/keyring/store.go`.
- `cache.go` — `Cache`: access-токены в памяти, с учётом expiry (в отличие
  от `keyring.Cache`, у которого истечения нет вовсе).
- `client.go` — `ExchangeCode`/`RefreshAccessToken`: RFC 6749 §4.1.3/§6,
  `application/x-www-form-urlencoded` (для этого в `internal/httpx`
  добавлен `PostForm` — токен-эндпоинты OAuth2, в отличие от всех прочих
  внешних вызовов в этом проекте, не принимают JSON).
- `service.go` — `Service`, единственная точка входа для
  `internal/httpapi` и `internal/mcp`: `StartAuthorization`,
  `CompleteAuthorization`, `AccessToken` (только из памяти, без сетевого
  I/O), `RefreshNow` (единственное осознанное исключение — блокирующий
  сетевой вызов, но только из фонового коннект-коллбэка, никогда с
  hot-path вызова тула), `HasToken`, `StartRefresher` (фоновый цикл,
  обновляет токены, у которых до истечения меньше 5 минут).

Важное ограничение, зафиксированное явно: ни один из существующих трёх
`MCPServerExtension`-механизмов (ключ шифрования, session-id, file-download
proxy) не делает сетевых вызовов внутри `executeTool` — только
подставляют уже готовое значение. Инъекция OAuth-токена следует тому же
правилу: `executeTool` берёт токен из `Cache` синхронно; если его там нет,
подключение сервера откладывается на следующий тик фонового цикла, а не
блокирует текущий вызов инструмента.

---

## 4. Сценарий первого входа

### 4.1 Tool `oauth_authorize`

Универсальный (не `authorize_google_calendar`, а `oauth_authorize` с
аргументом `provider`) — тот же принцип, что уже применяется к
`send_telegram`/`speak_reply`: один небольшой tool на функциональность, а
не один tool на каждую интеграцию (`../../internal/httpapi/agent_loop.go`,
блок `if o.oauth != nil { add(llm.ToolDef{Name: oauthAuthorizeToolName,
...}) }`).

`executeTool`'s `oauth_authorize`-обработчик вызывает
`Service.StartAuthorization`, возвращает ссылку в результате tool call (её
увидит/озвучит модель в своём ответе на любом текущем канале) и
дополнительно, если у пользователя есть известный Telegram-чат,
проактивно отправляет её туда же — иначе для голосового канала (`ha_assist`)
ссылка была бы бесполезна (её нельзя произнести).

### 4.2 Callback-роут

`GET {public_base_url}{callback_path}/{provider}`
(`../../internal/httpapi/oauth.go`, `Server.SetOAuthCallback` — тот же
`nil`-значит-выключено паттерн, что и `SetTelegramWebhook`). Единственный
"credential" этого роута — одноразовое значение `state`: сессионная cookie
здесь не проверяется (см. §2.1 обоснования в §3
`internal/oauth2/state.go`). `CompleteAuthorization` потребляет `state`
один раз (`PendingAuthStore.Consume`), обменивает `code` на токены и
рендерит статическую HTML-страницу с результатом.

---

## 5. `MCPServerExtension` — четвёртое расширение

По аналогии с `EncryptionKeyArg`/`SessionIDArg`/`FilesEndpoint`
(`../../internal/httpapi/orchestrator.go`) добавлены `OAuthProvider` и
`MCPServerURL`. В `executeTool` (`../../internal/httpapi/agent_loop.go`)
резолюция `toolServer, toolName, toolServerOK := o.tools.ServerAndTool(tc.Name)`
переиспользована как есть — четвёртая ветка проверяет
`toolExt.OAuthProvider != ""` и, если так, идёт через
`EnsureUserSession`+`CallForUser` вместо обычного `Call`.

Отдельно обработан `load_tool_group` (обязательное условие:
OAuth-gated сервер обязан быть `Lazy: true`, проверяется
`config.validateOAuthServers`) — без per-user сессии, поднятой заранее,
первый вызов `load_tool_group` для такого сервера отрапортовал бы успех,
хотя реальных тулов ещё нет. Решение: `load_tool_group`'s обработчик
проверяет `HasToken`, поднимает сессию через `EnsureUserSession` и ждёт её
готовности ограниченное время (`waitForUserSession`, до 5 секунд) — это
единственное осознанное исключение из правила "`executeTool` никогда не
блокируется на сети", ограниченное разовым случаем на пользователя за
время жизни процесса, а не ценой каждого вызова.

---

## 6. Конфигурация

```yaml
oauth:
  enabled: true
  public_base_url: "https://miranda.example.com"
  callback_path: "/oauth/callback"
  master_key_env: "OAUTH_MASTER_KEY"
  providers:
    - name: google_calendar
      description: "Google Calendar"
      authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID"
      client_secret_env: "GOOGLE_OAUTH_CLIENT_SECRET"
      scopes: ["https://www.googleapis.com/auth/calendar"]
      pkce: true
      extra_authorize_params: { access_type: offline, prompt: consent }
```

Никакой `mcp.servers[]`-записи для `google_calendar` нет и не нужно (см.
обновление в начале документа) — этот `oauth.providers` вход потребляется
напрямую нативными `calendar_*`-тулами (`internal/calendar`,
`internal/httpapi/calendar_tools.go`), включённые уже одним
`oauth.enabled: true`.

`extra_authorize_params.access_type=offline` + `prompt=consent` — то, что
заставляет Google реально выдавать `refresh_token`: без этого повторное
согласие уже подключённого пользователя тихо его не возвращает.

`config.validateOAuthServers` (`../../internal/config/config.go`)
проверяет: `oauth_provider` и `token_env` взаимоисключающи;
OAuth-gated сервер обязан быть `lazy: true`; `oauth_provider` должен
ссылаться на существующего провайдера; у провайдера не должно быть
дублей по имени и обязаны быть заполнены `authorize_url`/`token_url`/
`client_id_env`/хотя бы один scope; `oauth.enabled: true` требует
`public_base_url`/`callback_path`/`master_key_env`.

---

## 7. Проверка после реализации

1. Юнит-тесты `internal/oauth2` (`go test ./internal/oauth2/...`) — PKCE
   против тестового вектора RFC 7636, одноразовость `state`, шифрование/
   расшифровка токенов, form-encoding запросов к токен-эндпоинту, полный
   цикл `StartAuthorization`→`CompleteAuthorization`→`AccessToken`,
   фоновый `StartRefresher`.
2. `internal/mcp` (`mcp_oauth_test.go`) — два разных `userID` против
   одного и того же OAuth-gated имени сервера получают независимые
   клиенты; вызов одного пользователя никогда не долетает до сессии
   другого.
3. `internal/httpapi` (`agent_loop_oauth_test.go`, `oauth_test.go`) —
   `oauth_authorize` возвращает ссылку и (при известном чате) шлёт её в
   Telegram; вызов `load_tool_group` для неавторизованного пользователя
   возвращает понятную ошибку без попытки поднять сессию; после
   `StartAuthorization`+`CompleteAuthorization` реальный вызов тула
   успешно доходит до заранее подставленного fake-клиента; callback-роут
   отклоняет отсутствующий/просроченный/повторно использованный `state`.
4. Ручная проверка с реальным Google Calendar MCP — см. чек-лист в плане
   реализации (регистрация OAuth-клиента в Google Cloud Console, redirect
   URI, сквозной проход через `oauth_authorize` → согласие → callback →
   реальный вызов `list_events`, независимость данных двух разных
   пользователей, отзыв доступа со стороны Google, устойчивость к
   перезапуску процесса).
