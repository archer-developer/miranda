# Ленивая загрузка MCP-тулов (`load_tool_group`)

> Статус: реализовано (2026-08-14). Ссылки на строки ниже соответствуют
> состоянию репозитория на момент написания документа и могли разойтись с
> кодом с тех пор — верь коду, не этому файлу. `sandbox`/`diary`/`yazio`/
> `medical_card` помечены `lazy: true` в `../../config/mcp.yaml`; `ha`
> остаётся всегда включённым (см. §4).

---

## 1. Проблема

Каждый ход агентного цикла (`../../internal/httpapi/orchestrator.go:436`,
`o.availableTools(ctx)`) собирает **полный** список тулов — все включённые
MCP-серверы целиком (`../../internal/mcp/mcp.go`'s `Manager.Tools()`, вызывается
из `availableTools`, `../../internal/httpapi/agent_loop.go`, конец функции —
`mcpTools := o.tools.Tools(ctx)`) плюс все встроенные тулы — и отправляет их
целиком в модель на **каждой** из до `maxToolIterations = 15`
(`../../internal/httpapi/orchestrator.go:28`) итераций одного хода, независимо от
того, о чём вопрос.

Снимок реального прод-трафика (см. `../../system-prompt.md`, тот же conversation,
`archer@miranda:~/miranda/logs/llm.log`, `2026-08-13T17:52:29Z`..`17:55:06Z`):

- 66 схем тулов на каждый вызов: yazio×12, medical_card×9, ha_*×20, sandbox×6,
  diary×3, plus ~16 встроенных.
- `promptTokenCount` за 5 вызовов одного разговора: 12857 → 15574 → 16780 →
  16935 → 18001 — то есть ≈13-18K токенов чистого оверхеда на схемы тулов
  пересылаются заново на каждом шаге, независимо от того, что реально нужно
  ответить (вопрос был только про медкарту — yazio/sandbox/diary тулы ни разу
  не понадобились).
- Основной провайдер (`default_provider: gemini-lite` в `../../config/llm.yaml`,
  наиболее дешёвый/слабый тир) не поддерживает переиспользование этого
  контекста между вызовами — см. `docs/adr/gemini-context-caching.md` — то
  есть весь этот объём реально гоняется по сети и оплачивается на каждом шаге.

Отдельно от стоимости — это риск качества выбора тула: точность
tool-calling у LLM заметно проседает с ростом числа одновременно видимых
функций (это не проблема размера контекстного окна — окна на порядки
больше — а проблема того, что модели физически труднее выбрать правильную
функцию/аргументы из большого плоского списка). Проект активно растёт (см.
`../../CLAUDE.md`: «не просто HA voice add-on»), 66 — не потолок.

Из 66 тулов на подавляющем большинстве ходов реально нужны только HA-тулы
(20, ядро голосового ассистента умного дома) и встроенные core-тулы (~16:
`remember_this`, `speak_reply`, `end_conversation`, web-search/fetch,
schedule, telegram). Питание (yazio), медкарта (medical_card), песочница
(sandbox) и дневник (diary) — 30 тулов суммарно — релевантны только для
конкретных, редких по сравнению с общим потоком, запросов.

---

## 2. Идея: один тул-заглушка вместо четырёх доменов

Аналог механизма, который уже реально используется в этой самой среде
разработки для отложенных MCP-тулов (`ToolSearch` над
`mcp__claude-in-chrome__*`/`mcp__idea__*`): вместо того чтобы всегда
показывать модели полные схемы редко нужных доменов, показываем **один**
тул `load_tool_group(group)` с одной строкой описания на каждый ещё не
загруженный домен. Модель вызывает его, когда решает, что домен нужен —
локально (без сетевого похода к самому MCP-серверу) это разворачивается в
реальные схемы этого домена для **оставшихся итераций текущего хода**.

### 2.1 Конфигурация (`../../internal/config/config.go`)

По аналогии с `MCPServer.EncryptionKeyAllowed`/`ExposeFiles` (строки
443-478) — ещё два поля в той же структуре:

```go
// Lazy opts this server's tools out of every turn's default tool list.
// Instead of MCPServer's own real tool schemas, availableTools shows the
// model a one-line entry inside the shared load_tool_group stub tool
// (Description below); the real schemas only appear once the model calls
// load_tool_group with this server's Name, for the rest of that turn's
// tool-call loop. Servers used on most turns (e.g. ha) should leave this
// false — the point is to hide tools that are relevant to a minority of
// requests, not to add a round-trip to the common case.
Lazy bool `yaml:"lazy,omitempty"`
// Description is the one-line, model-facing summary of what this server's
// tools are for — shown inside load_tool_group's own parameter
// description when Lazy is true. Required when Lazy is true (checked at
// config-load time); meaningless otherwise.
Description string `yaml:"description,omitempty"`
```

Пример для `../../config/mcp.yaml` (сервер `ha` явно не ленивый — это
основной сценарий использования, ему не нужен лишний ход в цикле):

```yaml
mcp:
  servers:
    - name: ha
      url: "http://192.168.1.50:8123/api/mcp"
      enabled: true
      # lazy не указан — false, тулы всегда в списке

    - name: yazio
      url: "http://127.0.0.1:8790/mcp"
      enabled: true
      lazy: true
      description: "Учёт питания и калорий (YAZIO): поиск продуктов, запись съеденного, дневная сводка КБЖУ, рецепты."

    - name: medical_card
      url: "https://127.0.0.1:8791/mcp"
      enabled: true
      expose_files: true
      lazy: true
      description: "Медкарта: анализы, диагнозы, лекарства, документы, хронология событий здоровья."

    - name: code_exec_sandbox
      url: "http://127.0.0.1:8788/mcp"
      enabled: true
      expose_files: true
      lazy: true
      description: "Выполнение кода (bash/python) в песочнице: расчёты, обработка файлов, генерация PDF."

    - name: diary
      url: "https://localhost:8789/mcp"
      enabled: true
      encryption_key_allowed: true
      encryption_key_arg: record_encryption_key
      lazy: true
      description: "Личный дневник: заметки, мысли, события — поиск по смыслу."
```

Валидация при загрузке конфига (рядом с `validateMCPServerNames`,
`validateEncryptionKeyServers`): `Description` не пусто, если `Lazy: true`
— иначе `load_tool_group`'s enum-параметр получит домен без объяснения,
зачем он нужен, и модель не сможет осмысленно решить, стоит ли его
загружать.

### 2.2 Известное ограничение: описание не выводится из реальных тулов

`Description` — статическая строка в конфиге, а не производная от
фактического списка тулов сервера (в отличие от `Manager.Tools()`,
который каждый раз честно спрашивает сервер заново). Если на
`medical_card` добавят новый тул, `load_tool_group`'s описание домена
`medical_card` само по себе не изменится, пока кто-то не обновит
`description` в `mcp.yaml` руками. Это принимаемый компромисс — то же
самое ограничение, что и у любого human-written summary, а не баг
конкретно этого дизайна.

### 2.3 Новый метод `mcp.Manager` — тулы одного сервера

`Manager.Tools()` (`../../internal/mcp/mcp.go`) сейчас отдаёт всё плоским
списком сразу под префиксованными именами. Для разворачивания одного
домена нужен более узкий метод:

```go
// ToolsForServer returns just one server's own tools, prefixed exactly
// like Tools() does — the subset composeTools splices in once that
// server's group has been loaded via load_tool_group. Unlike Tools(), it
// doesn't aggregate across every configured server.
func (m *Manager) ToolsForServer(ctx context.Context, name string) []llm.ToolDef
```

Реализация — то же, что уже происходит внутри `Tools()` для одного
конкретного сервера (тот же RPC `tools/list` к тому же `mcpClient`,
просто не в цикле по всем серверам).

### 2.4 `turnControl` — состояние загруженных доменов на ход

`turnControl` (`../../internal/httpapi/agent_loop.go:35-50`) уже несёт
флаги, которые `executeTool` выставляет, а `Handle`/`runAgentLoop` читают
после цикла (`endRequested`, `forgetRequested`). Добавить туда же:

```go
type turnControl struct {
    endRequested    bool
    forgetRequested bool
    // loadedGroups accumulates which lazy MCP server names load_tool_group
    // has expanded so far THIS turn — reset implicitly every turn since
    // turnControl is constructed fresh in Handle (orchestrator.go:438).
    // Never persisted to history/memory: re-collapsing to the compact stub
    // on the next turn is intentional, not a bug — see §3.
    loadedGroups map[string]bool
    // groupsChanged is set by executeTool whenever loadedGroups grew this
    // iteration, so runAgentLoop knows to recompute the tool list before
    // the next call instead of doing it unconditionally every iteration.
    groupsChanged bool

    downloadedFiles []downloadedFile
    remoteFileURLs  map[string]bool
}
```

### 2.5 `executeTool` — обработка `load_tool_group`

Рядом с существующими локальными тулами (`rememberToolName` на строке 831
и далее, тот же `if tc.Name == ... { ... }` стиль):

```go
if tc.Name == loadToolGroupToolName {
    var args struct {
        Group string `json:"group"`
    }
    if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
        return fmt.Sprintf("error: invalid arguments: %v", err)
    }
    if _, ok := o.lazyServerDescriptions[args.Group]; !ok {
        return fmt.Sprintf("error: unknown tool group %q", args.Group)
    }
    if control.loadedGroups == nil {
        control.loadedGroups = map[string]bool{}
    }
    control.loadedGroups[args.Group] = true
    control.groupsChanged = true
    return fmt.Sprintf("tools for %q are now available — call the specific tool you need now", args.Group)
}
```

`o.lazyServerDescriptions map[string]string` — заполняется на старте из
конфига, тем же способом, что и `o.sessionIDAllowed`/`o.encryptionKeyAllowed`
(см. `docs/medical-card-session-injection.md §2.5` за образцом wiring из
`../../cmd/miranda/main.go` через `orchestrator.SetLazyMCPServers(...)`).

### 2.6 `availableTools` → `composeTools` — сборка списка с учётом состояния

Сейчас `availableTools(ctx)` вызывается один раз до цикла
(`orchestrator.go:436`) и передаётся в `runAgentLoop` статическим
параметром `tools []llm.ToolDef`. Чтобы список рос по ходу цикла, сборка
должна учитывать `control.loadedGroups`, а не быть константой одного
вызова:

```go
// composeTools builds this iteration's tool list: every non-lazy tool
// (built-ins + non-lazy MCP servers, e.g. ha) always included, plus —
// for each lazy server — either its real tools (if control.loadedGroups
// says it's been requested this turn) or nothing, replaced instead by one
// shared load_tool_group stub covering every lazy server NOT yet loaded.
// Once every lazy server has been loaded, the stub disappears entirely —
// nothing left to lazily load.
func (o *Orchestrator) composeTools(ctx context.Context, control *turnControl) []llm.ToolDef {
    out := append([]llm.ToolDef{}, o.baselineTools...) // built-ins + non-lazy MCP, computed once at startup-ish cost like today's availableTools

    var pending []string
    for name := range o.lazyServerDescriptions {
        if control.loadedGroups[name] {
            out = append(out, o.tools.ToolsForServer(ctx, name)...)
        } else {
            pending = append(pending, name)
        }
    }
    if len(pending) > 0 {
        out = append(out, o.loadToolGroupStub(pending))
    }
    return out
}
```

`loadToolGroupStub(pending []string)` строит один `llm.ToolDef`:

```go
func (o *Orchestrator) loadToolGroupStub(pending []string) llm.ToolDef {
    sort.Strings(pending) // deterministic order across calls — stable prompt for identical state
    var desc strings.Builder
    desc.WriteString("Load the real tools for one of these domains before calling anything in it — you currently only see this one-line summary, not their actual tool schemas:\n")
    for _, name := range pending {
        fmt.Fprintf(&desc, "- %s: %s\n", name, o.lazyServerDescriptions[name])
    }
    return llm.ToolDef{
        Name:        loadToolGroupToolName,
        Description: desc.String(),
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "group": map[string]any{"type": "string", "enum": pending},
            },
            "required": []string{"group"},
        },
    }
}
```

### 2.7 Правка `runAgentLoop` — перестроение списка после `load_tool_group`

`orchestrator.go:436` вызывает `composeTools` один раз (с пустым
`control.loadedGroups`) для первой итерации; внутри цикла
(`agent_loop.go:695-727`) — пересчёт только когда что-то реально
изменилось:

```go
for i := 0; i < maxToolIterations; i++ {
    text, toolCalls, err := o.streamOneTurn(ctx, source, messages, tools, &providerUsed)
    ...
    for _, tc := range toolCalls {
        result := o.executeTool(ctx, userID, conversationID, tc, control)
        ...
    }
    if control.groupsChanged {
        tools = o.composeTools(ctx, control)
        control.groupsChanged = false
    }
}
```

Один `if` в существующем цикле — `tools` до сих пор параметр, просто
теперь переприсваиваемый, а не только читаемый.

---

## 3. Почему не нужна защита от «вызова тула без предварительной загрузки»

Если модель когда-либо (например, из более раннего сообщения того же
длинного разговора, где домен уже был загружен на прошлой итерации, но
`tools` с тех пор пересобрался без него — не должно происходить по
дизайну §2.6, `loadedGroups` живёт весь ход) выдаст вызов реального тула
вроде `medical_card_medical.ask`, которого сейчас нет в `tools`,
`executeTool` всё равно успешно его выполнит: маршрутизация в
`mcp.Manager.Call` идёт по имени тула через `ServerAndTool`
(`../../internal/mcp/CLAUDE.md`), а не по тому, было ли имя в списке,
показанном модели именно в этот момент. Специальный код, запрещающий
«незаявленный» вызов, не нужен и не нужен — сам факт, что модель
корректно назвала существующий тул с правильными аргументами, означает,
что она это не выдумала, а помнит из контекста, и обслужить такой вызов
— правильное поведение, а не дыра, которую нужно закрывать.

---

## 4. Компромиссы

- **+1 ход в цикле на домен-специфичный запрос.** Вопрос про питание или
  медкарту теперь занимает 2 итерации вместо 1 (сначала
  `load_tool_group`, потом реальный вызов) — при `maxToolIterations = 15`
  запаса более чем достаточно даже если в одном ходе нужно два домена
  подряд (диетическая запись + дневник, например — 4 итерации). Задержка
  — одна пара запрос/ответ к самому дешёвому/быстрому провайдеру
  (`gemini-lite`), не к дорогому.
- **`ha` остаётся всегда включённым.** Это основной домен голосового
  ассистента умного дома — лениво его загружать means лишний ход почти
  на каждом ходе, обратный эффект тому, что нужно.
- **`description` требует ручного сопровождения** (см. §2.2) — не
  автоматизировано, и не должно быть: одна строка руками при добавлении
  нового MCP-сервера — разумная цена за читаемость для модели.
- **Состояние `loadedGroups` не переживает ход.** Каждый новый
  пользовательский вопрос начинается со свёрнутого набора, даже если
  предыдущий вопрос в том же разговоре уже загружал `medical_card`. Это
  сознательный выбор простоты (см. doc-comment в §2.4) — переносить
  состояние между ходами добавило бы ещё один источник рассинхрона
  (например, если конфиг между ходами поменяли) ради экономии одного хода
  раз в несколько сообщений подряд про один и тот же домен.

---

## 5. Проверка после реализации

1. Обычный вопрос про умный дом («выключи свет в зале») — в
   `logs/llm.log` (см. `../../CLAUDE.md`'s "Logging") виден только базовый
   набор (built-ins + `ha_*` + один `load_tool_group` с 4 доменами в
   enum), не все 66 — сравнить `promptTokenCount` с `../../system-prompt.md`'s
   исходным снимком (12857 на сопоставимый по длине системный промпт) —
   должно быть заметно меньше.
2. Вопрос про медкарту (тот же сценарий, что дал текущий снимок,
   «Найди в медкарте УЗИ щитовидной железы...») — первый вызов содержит
   `load_tool_group` с 4 pending-доменами; второй вызов (после того, как
   модель его вызвала) содержит все 9 `medical_card_*` тулов и уже НЕ
   содержит `yazio_*`/`code_exec_sandbox_*`/`diary_*` схем, только
   сокращённый `load_tool_group` с 3 оставшимися доменами (или тул вовсе
   пропадает, если это был последний).
3. Запрос, требующий двух доменов подряд в одном ходе (например, «запиши
   этот рецепт в дневник и посчитай его КБЖУ») — цикл укладывается в
   `maxToolIterations`, оба домена реально разворачиваются, финальный
   ответ приходит.
4. Модель, вызвавшая существующий, но не загруженный в этом ходе тул
   напрямую (см. §3) — вызов всё равно успешно выполняется, не падает с
   «unknown tool».
