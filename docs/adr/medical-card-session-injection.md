# Автоматическая подстановка `sessionId` в `medical.ask`

> Статус: реализовано (`../../internal/config`, `../../internal/mcp`, `../../internal/httpapi`, `../../cmd/miranda`,
> `../../config/mcp.yaml`). Итоговый код местами отличается от псевдокода ниже в деталях именования
> (например, `SessionIDServer`/`ServerAndTool` экспортированы, поскольку `../../cmd/miranda` строит их
> напрямую) — если что-то из описанного ниже разойдётся с кодом в будущем, верь коду, не этому файлу.

---

## 1. Проблема

`miranda-medical-card`'s `medical.ask` (см. `miranda-medical-card/docs/adr/001-internal-agent-loop-implementation.md`, `miranda-medical-card/docs/mcp/04-medical.md` §5, §7.1) теперь поддерживает необязательный параметр `sessionId`: если он передан, medical-card сохраняет историю вопросов/ответов и вызовов инструментов этой сессии в собственной SQLite и учитывает её при следующем вопросе с тем же `sessionId` — это то, что делает уточняющие вопросы ("а что насчёт анализов сына?") осмысленными без повторения полного контекста в каждом вопросе.

Идея (см. тот ADR, §2): Miranda должна передавать **своё уже разрешённое** `conversationID` (то же самое значение, что уже используется для `llmtrace.WithConversationID` и передаётся в `runAgentLoop`, см. `../../internal/httpapi/orchestrator.go`'s `resolveConversation`) — так гарантируется, что сессия Miranda и внутренняя сессия медкарты остаются синхронизированы 1:1.

**Сейчас этого не происходит.** `sessionId` — обычный параметр в JSON Schema тула `medical.ask`, как и любой другой аргумент MCP-вызова: значение для него, если оно вообще появляется, должна написать сама модель внутри своего tool call (см. `../../internal/httpapi/agent_loop.go`'s `executeTool`, `o.tools.Call(ctx, tc.Name, callArgs)` — `callArgs` в норме приходит из `tc.Arguments`, то есть из того, что написала модель). У модели нет доступа к `conversationID` как к значению, которое она может просто вписать в аргумент — это внутренняя деталь оркестратора, не часть контекста модели. В результате:

- либо модель никогда не передаёт `sessionId` — и вся функциональность сессий медкарты простаивает вхолостую;
- либо (хуже) модель, увидев параметр в схеме тула, может попытаться придумать правдоподобно выглядящее значение самостоятельно — medical-card's `AskInput.SessionID`'s jsonschema-описание уже прямо просит этого не делать ("never invent or guess a value for this field yourself"), но полагаться на то, что модель всегда послушается инструкции в описании поля, не годится как единственная защита.

Корень проблемы — тот же, что уже был решён для ключа шифрования miranda-diary (см. `miranda-medical-card/docs/file-staging-refactor.md`-style прецедент в этом же репозитории, `encryption.md`): **передача внутреннего идентификатора между Miranda и внешним MCP-сервисом — механическая задача, а не задача, требующая понимания языка**, и её должен делать backend, перехватывая исходящий вызов, а не модель.

---

## 2. Идея: подстановка по аналогии с `setEncryptionKeyArg`

Тот же паттерн, что уже используется для ключа шифрования (`internal/httpapi/agent_loop.go:1032-1057`, `setEncryptionKeyArg` на строке 1158), с двумя упрощениями: `sessionId` не секрет (не нужна проверка HTTPS-only, как для `EncryptionKeyPermitted`), и подставлять его имеет смысл только для конкретных тулов конкретного сервера (не для всех тулов сервера разом, как с ключом шифрования — см. §3).

### 2.1 Конфигурация (`../../internal/config/config.go`)

По аналогии с `MCPServer.EncryptionKeyAllowed`/`EncryptionKeyArgName` (строки 443-465) добавить:

```go
// SessionIDArgName overrides the tool-call argument name this server's
// schema expects Miranda's own resolved conversation id under. Left
// unset, defaultSessionIDArgName ("sessionId") is used.
SessionIDArgName string `yaml:"session_id_arg,omitempty"`
// SessionIDTools lists which of this server's own (unprefixed) tool
// names should receive the injected conversation id — unlike
// EncryptionKeyAllowed, this is NOT server-wide: most tools on a given
// MCP server won't declare a sessionId parameter in their own schema at
// all (e.g. miranda-medical-card's medical.profile, medical.timeline),
// and injecting an argument a tool's schema doesn't expect is exactly
// the kind of mismatch that made EncryptionKeyArgName necessary in the
// first place — see that field's own doc comment on the diary incident.
SessionIDTools []string `yaml:"session_id_tools,omitempty"`
```

По образцу `defaultEncryptionKeyArgName`/`MCPServer.EncryptionKeyArg()` (строки 486-498)
завести такую же пару для sessionId — это то, на что реально опирается wiring в §2.5
(`s.SessionIDArg()`, а не голое чтение `SessionIDArgName`):

```go
// defaultSessionIDArgName is the tool-call argument name used for a server
// that lists itself in SessionIDTools without overriding SessionIDArgName.
const defaultSessionIDArgName = "sessionId"

// SessionIDArg returns the tool-call argument name this server's schema
// expects Miranda's own resolved conversation id under: SessionIDArgName if
// set, else defaultSessionIDArgName — same override-or-default shape as
// MCPServer.EncryptionKeyArg().
func (s MCPServer) SessionIDArg() string {
    if s.SessionIDArgName != "" {
        return s.SessionIDArgName
    }
    return defaultSessionIDArgName
}
```

Пример для medical-card в `../../config/mcp.yaml` (или где у Miranda хранится конфигурация серверов):

```yaml
mcp:
  servers:
    - name: medical_card
      url: "https://..."
      session_id_tools: ["medical.ask"]
      # session_id_arg не указан — используется defaultSessionIDArgName "sessionId"
```

Валидация не нужна в том объёме, что для ключа шифрования (нет условия "только HTTPS") — но стоит проверить при загрузке конфига, что `session_id_tools` не пуст, если задан `session_id_arg`, и наоборот (бессмысленная неполная конфигурация), аналогично духу `validateEncryptionKeyServers`.

### 2.2 Пробрасывание `conversationID` до `executeTool`

`executeTool` (строка 815) сейчас не принимает `conversationID` — добавить параметр:

```go
func (o *Orchestrator) executeTool(ctx context.Context, userID, conversationID string, tc llm.ToolCall, control *turnControl) string
```

и обновить единственный вызов на строке 705 (`runAgentLoop` уже имеет `conversationID` в своей сигнатуре — см. строку ~680):

```go
result := o.executeTool(ctx, userID, conversationID, tc, control)
```

### 2.3 Предпосылка: `mcp.Manager` должен отдавать непрефиксованное имя тула

`o.tools` (тип `*mcp.Manager`, `../../internal/mcp/mcp.go`) уже умеет резолвить владеющий
сервер по префиксованному имени — `ServerForTool` (строка 218) — но не сам тул: имя
без префикса собирается/разбирается только через `prefixedToolName` (строка 36), а
эта функция **не экспортирована** и используется исключительно внутри пакета `mcp`
(`Tools`, `Call`, приватный `serverForTool`). В отличие от `encryptionKeyAllowed`,
которому имя конкретного тула не требуется вовсе (разрешение — на весь сервер),
инъекция `sessionId` обязана знать и сервер, и голое имя тула на нём (см. §3) — значит,
`httpapi` не может обойтись одним `ServerForTool`.

Дублировать конвенцию `"<server>_" + tool` прямо в `httpapi` — то же самое нарушение,
которого сама `prefixedToolName` призвана избежать (её doc-comment: "the single source
of truth ... so the format can't drift between where it's built and where it's parsed
back apart"). Поэтому первым шагом — расширить `internal/mcp.Manager` новым
экспортируемым методом, а не звать `prefixedToolName` напрямую из другого пакета:

```go
// ServerAndTool returns which server owns prefixedName (as produced by
// Tools) and that tool's own, unprefixed name — the extended form of
// ServerForTool needed by callers (e.g. session-id injection, see
// docs/medical-card-session-injection.md) that must know the bare tool name,
// not just its owning server.
func (m *Manager) ServerAndTool(prefixedName string) (server, tool string, ok bool) {
    for _, name := range m.orderSnapshot() {
        prefix := prefixedToolName(name, "")
        if strings.HasPrefix(prefixedName, prefix) {
            return name, strings.TrimPrefix(prefixedName, prefix), true
        }
    }
    return "", "", false
}
```

(`ServerForTool` (строка 218) можно оставить как есть — оба метода делят один и тот же
проход по `m.orderSnapshot()`/`prefixedToolName`, ничего не дублируют друг у друга по
сути, просто `ServerAndTool` — более широкий интерфейс для нового вызывающего кода.)

### 2.4 Инъекция аргумента

По образцу блока `if o.keyring != nil { ... }` (строки 1032-1057), добавить рядом (или сразу после) аналогичный блок, используя `ServerAndTool` из §2.3 вместо `ServerForTool` + ручного `TrimPrefix`:

```go
if server, tool, ok := o.tools.ServerAndTool(tc.Name); ok {
    if entry, allowed := o.sessionIDAllowed[server]; allowed && entry.tools[tool] {
        var setOK bool
        callArgs, setOK = setSessionIDArg(callArgs, entry.argName, conversationID)
        if !setOK {
            o.hub.Publish(hub.Event{Source: "error", Message: "session id not attached to " + tc.Name + ": arguments were not valid JSON"})
        }
    }
}
```

(`o.sessionIDAllowed` — `map[string]sessionIDServer`, где

```go
// sessionIDServer is one MCP server's session-id injection permission: the
// resolved argument name (MCPServer.SessionIDArg()) plus the set of that
// server's own (unprefixed) tool names allowed to receive it.
type sessionIDServer struct {
    argName string
    tools   map[string]bool
}
```

— в отличие от `encryptionKeyAllowed map[string]string`, которое смотрит только на
сервер, здесь разрешение проверяется и по серверу, и по имени конкретного тула на
нём, поэтому одного `map[string]string` недостаточно.)

`setSessionIDArg` — почти буквальная копия `setEncryptionKeyArg` (строка 1158), с двумя отличиями: работает со строкой `conversationID`, а не `[]byte` ключом, и **всегда перезаписывает** значение (никогда не "strip" при пустом значении, поскольку `conversationID` внутри `executeTool` по построению всегда непусто — `runAgentLoop` уже гарантированно его разрешил до первой итерации, см. `resolveConversation`):

```go
// setSessionIDArg sets argName to sessionID in args' JSON, unconditionally
// overwriting any value the model may have set itself — see
// docs/medical-card-session-injection.md §1 for why the model must never
// be trusted to supply this value on its own.
func setSessionIDArg(args, argName, sessionID string) (result string, ok bool) {
    m, decodeOK := decodeToolArgs(args)
    if !decodeOK {
        return args, false
    }
    m[argName] = sessionID
    return encodeToolArgs(m, args), true
}
```

(`decodeToolArgs`/`encodeToolArgs` уже существуют, строки 1174-1192 — переиспользовать как есть.)

### 2.5 Wiring в `../../cmd/miranda/main.go`

По аналогии с `encryptionKeyAllowedServers(cfg.MCP.Servers, logger)` → `orchestrator.SetEncryptionKeyAllowedServers(...)` (строка 304, использует `s.EncryptionKeyArg()` для резолва имени аргумента — строка 574): построить аналогичную функцию `sessionIDAllowedServers(cfg.MCP.Servers) map[string]sessionIDServer` (сервер → `sessionIDServer{argName: s.SessionIDArg(), tools: ...}`, где `argName` берётся через аксессор из §2.1, а не напрямую из `SessionIDArgName`) и передать через новый `orchestrator.SetSessionIDAllowedServers(...)`.

### 2.6 Общность механизма

Ничего в §2.1-2.5 не завязано на medical-card конкретно — `medical_card`/`medical.ask` в
этом документе встречаются только в мотивации (§1), обосновании per-tool scope (§3) и
контракте (§4), но не в самом коде: конфигурация читается со всех `cfg.MCP.Servers`
одинаково (`sessionIDAllowedServers` не знает имён серверов заранее), инъекция в
`executeTool` решает только по содержимому `o.sessionIDAllowed` (построенному из
конфига), а `ServerAndTool` разбирает префикс механически, не заглядывая в то, что за
сервер/тул перед ним. Это значит: включить ту же подстановку `conversationID` для
любого другого MCP-сервера и любого набора его тулов в будущем — вопрос строки в
`../../config/mcp.yaml` (`session_id_tools: [...]`, при необходимости `session_id_arg: ...`),
без единой правки Go-кода.

---

## 3. Почему scope — по тулу, а не по серверу

`EncryptionKeyAllowed` — булев флаг на весь сервер, потому что у miranda-diary **каждый** тул работает с зашифрованными записями и ожидает ключ. Это не так для medical-card: `sessionId` объявлен только в схеме `medical.ask` (см. `miranda-medical-card/internal/mcpserver/ask.go`'s `AskInput`) — `medical.profile`, `medical.timeline`, `medical.upload_document` и другие тулы этого же сервера такого поля не ожидают вовсе. Если подставлять `sessionId` в аргументы **любого** вызова к серверу `medical_card` (по образцу `encryptionKeyAllowed`, который смотрит только на имя сервера), каждый вызов любого другого тула этого сервера получит лишнее поле в JSON — и, в зависимости от того, насколько строго MCP SDK на стороне medical-card валидирует схему (`additionalProperties`), это либо будет молча проигнорировано, либо (что как раз и произошло в §2 `file-staging-refactor.md`'s истории про ключ шифрования на другом сервере) сломает вызов ошибкой валидации схемы. Поэтому `SessionIDTools` — явный список имён тулов, а не флаг на весь сервер.

---

## 4. Контракт со стороны medical-card (не меняется, зафиксирован)

- Тул: `medical.ask`.
- Аргумент: `sessionId` (string, JSON), см. `AskInput.SessionID` в `miranda-medical-card/internal/mcpserver/ask.go`.
- Значение: непрозрачная строка-идентификатор. medical-card ничего не знает о её внутренней структуре — только использует её как первичный ключ таблицы `ask_sessions`. Рекомендуется передавать `conversationID` как есть, без преобразований.
- Отсутствие поля — валидный, полностью поддерживаемый режим (`medical.ask` ведёт себя как одноразовый, не сохраняющий контекст вызов) — не нужно передавать пустую строку или синтетическое значение, когда `conversationID` неизвестен (например, если когда-либо появится вызов `medical.ask` вне контекста активной беседы).
- Идемпотентность: повторный вызов с тем же `sessionId` безопасен и ожидаем (это и есть механизм уточняющих вопросов) — не нужно генерировать новый `sessionId` на каждый вызов одной и той же беседы.
- Устаревание сессии: medical-card сама считает сессию, к которой давно не обращались, устаревшей (см. `miranda-medical-card/internal/ask/session.go`'s `sessionIdleTTL`) — Miranda не обязана ничего чистить или инвалидировать на своей стороне сверх обычного жизненного цикла `conversationID`.

---

## 5. Проверка после реализации

1. Сценарий: вопрос пользователя → Miranda вызывает `medical_card_medical.ask` без явного `sessionId` в аргументах модели → в `../../logs/llm.log` (или эквивалентном трейсе вызова тула) видно, что реально отправленный `sessionId` равен `conversationID` этой беседы.
2. Follow-up вопрос в той же беседе → второй вызов `medical.ask` с тем же `sessionId` → ответ демонстрирует использование контекста первого вопроса (см. `miranda-medical-card/internal/ask/agent_loop_test.go`'s `TestAsk_SessionContinuity_ReplaysHistoryIntoSecondCall` за примером того, что именно должно происходить на стороне medical-card).
3. Вызов любого другого тула сервера `medical_card` (например `medical.profile`) не содержит внедрённого `sessionId` — подтверждает, что scope ограничен `SessionIDTools`, а не всем сервером.
4. Модель, попытавшаяся сама вписать `sessionId` в аргументы `medical.ask`, не может повлиять на итоговое значение — `setSessionIDArg` всегда перезаписывает.
