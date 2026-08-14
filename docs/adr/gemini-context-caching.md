# Кэширование контекста для Gemini (`CachedContent`)

> Статус: предложено, не реализовано. Код, который это меняет, живёт не в
> этом репозитории, а в `github.com/archer-developer/miranda-llm`
> (Miranda подключает его как модуль — см. `../../go.mod`, версия на момент
> написания `v0.0.0-20260813111120-dd5ccfb8024d`). Пути ниже — относительно
> корня *того* репозитория, не `miranda`. Если разойдётся с кодом в будущем
> — верь коду, не этому файлу.

---

## 1. Проблема

`miranda-llm/gemini.ToolsConfig.ContextCaching` (`gemini/gemini.go:71-77`) уже
существует как конфигурационный флаг — и Miranda уже готова его включить
(`../../internal/config/config.go:355-361`, `GeminiToolsConfig.ContextCaching`,
`yaml:"context_caching"`) — но `gemini.New` сейчас **отклоняет запуск**, если
он `true`:

```go
// gemini/gemini.go:115-122
if tools.ContextCaching {
    return nil, fmt.Errorf("gemini: context_caching is not implemented yet (see ToolsConfig doc comment) — leave it false")
}
```

Комментарий на поле честно объясняет, почему это не тривиальный флаг:

> Gemini's `CachedContent` is an explicit, separately-managed resource
> (create/reference/invalidate), structurally unlike Anthropic's per-request
> `cache_control` breakpoint, and needs its own design pass (cache-key
> strategy, invalidation trigger, TTL policy) that doesn't belong bolted
> onto this field.

Это — тот самый design pass. Мотивация в цифрах: см.
`../../system-prompt.md` и `docs/adr/lazy-mcp-tool-loading.md §1` — тот же
разговор из `logs/llm.log`, ~13-18K токенов `promptTokenCount` на **каждом**
из 5 вызовов за 3 минуты, потому что `default_provider: gemini-lite`
(`../../config/llm.yaml`) пересылает полный `systemInstruction` + `tools`
заново на каждый шаг без переиспользования. Anthropic-провайдер (`claude`,
верхняя ступень эскалации) от этого уже частично защищён —
`anthropic.go`'s `toAnthropicMessages` ставит `cache_control` breakpoint на
**первый** system-блок (см. `docs/adr/system-prompt-caching.md` — отдельная,
уже реализованная правка, разбита на стабильный/волатильный блоки именно
для того, чтобы этот breakpoint вообще имел смысл) — но это не основной
путь трафика: `claude` — верхняя ступень эскалации, а не `default_provider`.

---

## 2. Почему нельзя просто скопировать anthropic.go's приём

Anthropic-подход (`toAnthropicMessages`) — **мягкий**: breakpoint помечает
«всё до сюда — переиспользуй, если совпадёт», а если содержимое разошлось,
Anthropic просто не находит совпадения и молча считает по полной цене для
несовпавшего хвоста. Ошибки нет, деградация плавная — на этой мягкости и
держится `docs/adr/system-prompt-caching.md`'s решение звать `buildSystemPrompt`
каждый ход заново, не думая об инвалидации в явном виде.

`CachedContent` у Gemini — **явный** ресурс: его нужно создать заранее
(`Caches.Create` — точная сигнатура в `google.golang.org/genai` requires
проверки на момент реализации, см. §6), получить его имя, и затем сослаться
на него по имени в `GenerateContentConfig`. Если содержимое запроса
разошлось с тем, что лежит в кэше, никакого «частичного» переиспользования
нет — либо используешь ровно то, что закэшировано, либо не используешь кэш
вообще для этого запроса.

**После `docs/adr/system-prompt-caching.md` у Miranda уже есть подходящая
структура для этого** — `buildSystemPrompt`
(`../../internal/httpapi/agent_loop.go:439`) возвращает `(stable, volatile
string)`, `Handle` (`../../internal/httpapi/orchestrator.go:437-449`) шлёт их
как два `RoleSystem`-сообщения. `stable` (персона + спикер + shared/personal
память) больше не меняется на каждый вызов — память теперь читается один
раз на разговор (`Orchestrator.conversationMemory`), так что `stable` внутри
одного открытого разговора действительно побайтово неизменен от хода к
ходу, пока не сработает `remember_this`/summarization. Формально это
означало бы, что `stable`-блок в принципе годится под `CachedContent` не
хуже персоны у Anthropic.

Тем не менее для Gemini это **не тот же случай**, и вот почему кэшировать
`stable` пока не стоит, в отличие от `Tools`:

- `stable` стабилен **в рамках одного разговора одного пользователя**, а не
  глобально — у Anthropic это не проблема (мягкая деградация, breakpoint
  просто не совпадёт и спишется по полной цене без дополнительных действий
  с нашей стороны), но для Gemini's explicit-resource модели это означало
  бы **кэш на каждую (пользователь, разговор, API-ключ)** тройку, а не один
  глобальный кэш на процесс — на порядок больше ресурсов в управлении
  (создание/TTL/инвалидация на каждую тройку), первый ход разговора платит
  по полной цене за создание кэша, который окупится только если разговор
  окажется достаточно длинным (`toGeminiContents` и так уже собирает оба
  `RoleSystem`-сообщения в один `SystemInstruction` с несколькими `Part` —
  Gemini не разделяет их сам).
- `Tools`, наоборот, стабилен **глобально** — один процесс, одна
  конфигурация MCP-серверов, один кэш на (весь household, все разговоры,
  каждый API-ключ). Совпадение по хэшу почти гарантированно уже на втором
  вызове **любого** пользователя, а не только внутри одного и того же
  разговора.

**Решение остаётся прежним: кэшировать только `Tools`, никогда не
`SystemInstruction`.** Массив схем тулов абсолютно не зависит от
пользователя, разговора или памяти — при данной конфигурации MCP-серверов
он побайтово одинаков для всех пользователей, всех разговоров, всех ходов,
пока не изменится конфиг или набор включённых серверов. Это ровно то
свойство, которое нужно явному, все-или-ничего ресурсу вроде
`CachedContent`, и которого `SystemInstruction` — даже после разбиения на
`stable`/`volatile` — по-прежнему не имеет на глобальном уровне.
`SystemInstruction` при этом остаётся как сегодня — всегда пересылается
заново, инлайн, без кэша (это меньшая часть токенов — после
`docs/adr/lazy-mcp-tool-loading.md` в списке `Tools` тоже станет меньше на
общем ходу, но именно `SystemInstruction` был и остаётся маленьким по
сравнению с 66 схемами тулов). Кэширование `stable`-блока per-conversation
остаётся возможным future-follow-up, если разговоры на практике окажутся
достаточно длинными, чтобы окупить его собственную стоимость управления —
не требуется для v1.

Это решение не зависит от `docs/adr/lazy-mcp-tool-loading.md` — работает уже
сегодня, над нынешним статичным списком из 66 тулов (он и так побайтово
одинаков на каждый вызов). После того, как ленивая загрузка тулов будет
реализована, кэшируемый набор станет ещё компактнее (baseline + один
stub-тул `load_tool_group`) и будет реже меняться — см. §7.

---

## 3. Множественные API-ключи ломают наивную схему «один кэш на провайдера»

`gemini.Provider` держит `clients []*genai.Client` — **один клиент на
каждый настроенный API-ключ** (`gemini.go:91-99`, ротация — `pump`/`attempt`,
строки 178-289). `CachedContent`, однажды созданный через клиент с ключом
A, — ресурс в проекте/квоте этого конкретного ключа; ссылаться на его имя
через клиент с ключом B нет оснований ожидать, что сработает (другой
проект/квота).

Сейчас `Chat` строит **один** `cfg *genai.GenerateContentConfig` и
передаёт его без изменений во все попытки ротации:

```go
// gemini.go:165-176 (как есть)
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
    system, contents := toGeminiContents(req.Messages)
    cfg := &genai.GenerateContentConfig{
        SystemInstruction: system,
        Tools:             p.buildTools(req.Tools),
        Temperature:       toFloat32Ptr(req.Temperature),
    }
    out := make(chan llm.StreamChunk)
    go p.pump(ctx, contents, cfg, out)
    return out, nil
}

// pump -> attempt(ctx, p.clients[i], i, contents, cfg, out) — тот же cfg на каждый i
```

Если `cfg.Tools` заменить на `cfg.CachedContent = <имя кэша ключа 0>`
единожды в `Chat`, а `pump` в процессе ротации перейдёт на `clients[1]`
(квота/ошибка ключа 0) — `attempt` пошлёт запрос с чужим именем кэша.
Значит, ссылка на кэш должна разрешаться **не в `Chat`, а внутри
`attempt`, по конкретному `keyIndex`** — `cfg` из общего шаблона
(`SystemInstruction`, `Temperature`) превращается в шаблон, а `Tools`
(или `CachedContent`) дорешается на каждую попытку отдельно.

---

## 4. Дизайн

### 4.1 Состояние в `Provider`

```go
// gemini.go — новые поля Provider (gemini.go:91-99)
type Provider struct {
    name     string
    model    string
    clients  []*genai.Client
    tools    ToolsConfig
    rotation RotationConfig
    tracer   llm.Tracer
    logger   *slog.Logger

    // toolsCache tracks, per API key index, the CachedContent resource
    // currently holding that key's last-cached Tools array — nil entry (or
    // stale hash) means "no usable cache for this key right now, send
    // Tools inline". Guarded by cacheMu since Chat/attempt run
    // concurrently across streamed requests.
    cacheMu    sync.Mutex
    toolsCache []*cachedTools // len == len(clients), index-aligned
}

// cachedTools is one API key's cached Tools resource.
type cachedTools struct {
    hash      string    // hashTools(tools) at creation time
    name      string    // CachedContent resource name, e.g. "cachedContents/abc123"
    expiresAt time.Time
    creating  bool      // in-flight guard — see ensureCache
}
```

### 4.2 Хэш тулов — ключ кэша

```go
// hashTools returns a stable content hash of tools' canonical JSON
// encoding — used both to name/key the cache and to detect when the
// caller's tool list has drifted from what's currently cached (config
// reload, MCP server toggled, or — with lazy tool loading, see
// docs/adr/lazy-mcp-tool-loading.md in the miranda repo — a domain group
// loaded mid-turn) so a stale/mismatched cache is never referenced.
func hashTools(tools []*genai.Tool) string {
    b, _ := json.Marshal(tools) // field order stable: genai.Tool's own struct field order
    sum := sha256.Sum256(b)
    return hex.EncodeToString(sum[:])
}
```

### 4.3 `Chat`/`attempt` — резолвинг кэша на попытку, не на запрос

```go
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
    system, contents := toGeminiContents(req.Messages)
    tools := p.buildTools(req.Tools)
    cfg := &genai.GenerateContentConfig{
        SystemInstruction: system,
        Temperature:       toFloat32Ptr(req.Temperature),
        // Tools intentionally left unset here — attempt fills either
        // cfg.Tools (uncached path) or cfg.CachedContent (cache hit) per
        // key, since a CachedContent name is only valid for the key that
        // created it (see §3).
    }
    out := make(chan llm.StreamChunk)
    go p.pump(ctx, contents, tools, cfg, out)
    return out, nil
}

func (p *Provider) pump(ctx context.Context, contents []*genai.Content, tools []*genai.Tool, cfg *genai.GenerateContentConfig, out chan<- llm.StreamChunk) {
    ...
    err := keyrotation.Run(ctx, p.logger, "gemini", len(p.clients), rotCfg, isRetryable,
        func(ctx context.Context, i int) error {
            perKeyCfg := p.resolveToolsForAttempt(ctx, i, tools, *cfg) // shallow copy of cfg + Tools or CachedContent set
            return p.attempt(ctx, p.clients[i], i, contents, &perKeyCfg, out)
        },
    )
    ...
}

// resolveToolsForAttempt fills exactly one of cfg.Tools / cfg.CachedContent
// for key index i's attempt. Never blocks the live request on cache
// management: a cache miss, an in-flight creation, or any cache-API error
// all fall back to sending tools inline — correctness never depends on the
// cache being warm, only cost/latency does.
func (p *Provider) resolveToolsForAttempt(ctx context.Context, keyIndex int, tools []*genai.Tool, cfg genai.GenerateContentConfig) genai.GenerateContentConfig {
    if !p.tools.ContextCaching {
        cfg.Tools = tools
        return cfg
    }
    hash := hashTools(tools)
    if name, ok := p.usableCache(keyIndex, hash); ok {
        cfg.CachedContent = name
        return cfg
    }
    cfg.Tools = tools // uncached path — always correct
    p.ensureCacheAsync(keyIndex, hash, tools) // best-effort, non-blocking — see §4.4
    return cfg
}
```

### 4.4 Фоновое создание кэша — не в горячем пути

Создание `CachedContent` — отдельный API-вызов с собственной задержкой;
блокировать им живой пользовательский ход недопустимо (та же логика, что
у остальной обработки ошибок в этом коде: сбой вспомогательного механизма
не должен ронять или тормозить основной запрос).

```go
// ensureCacheAsync kicks off cache creation for (keyIndex, hash) in the
// background if nothing is already in flight for that pair — de-duplicates
// via the creating flag so concurrent requests during the same gap don't
// fire redundant Caches.Create calls. Never returns anything to the
// caller: the current request already fell back to inline tools (§4.3);
// this only helps the NEXT request with the same hash.
func (p *Provider) ensureCacheAsync(keyIndex int, hash string, tools []*genai.Tool) {
    p.cacheMu.Lock()
    entry := p.toolsCache[keyIndex]
    if entry != nil && (entry.hash == hash || entry.creating) {
        p.cacheMu.Unlock()
        return
    }
    p.toolsCache[keyIndex] = &cachedTools{hash: hash, creating: true}
    p.cacheMu.Unlock()

    go func() {
        // client.Caches.Create(...) — точная форма вызова в
        // google.golang.org/genai требует проверки на момент реализации,
        // см. §6. Ожидаемая форма: Tools-only cache (без Contents), TTL —
        // см. §5.
        name, expiresAt, err := p.createToolsCache(context.Background(), p.clients[keyIndex], tools)
        p.cacheMu.Lock()
        defer p.cacheMu.Unlock()
        if err != nil {
            p.logger.Warn("gemini: tools cache creation failed, will keep sending tools inline", "provider", p.name, "key_index", keyIndex, "error", err)
            p.toolsCache[keyIndex] = nil // clear the in-flight marker, allow a later retry
            return
        }
        p.toolsCache[keyIndex] = &cachedTools{hash: hash, name: name, expiresAt: expiresAt}
    }()
}

// usableCache returns the cached name for (keyIndex, hash) if one exists,
// isn't still being created, and hasn't passed its expiry.
func (p *Provider) usableCache(keyIndex int, hash string) (string, bool) {
    p.cacheMu.Lock()
    defer p.cacheMu.Unlock()
    entry := p.toolsCache[keyIndex]
    if entry == nil || entry.creating || entry.hash != hash || entry.name == "" {
        return "", false
    }
    if time.Now().After(entry.expiresAt) {
        return "", false
    }
    return entry.name, true
}
```

### 4.5 TTL и обновление

Предлагается скользящий TTL порядка **30 минут** — чуть больше
`Memory.SessionIdleTimeoutMinutes` (по умолчанию 25,
`../../internal/config/config.go`), чтобы активная домашняя сессия почти
никогда не видела истёкший кэш, а простаивающий (ночь, никого дома)
переставал стоить денег за хранение вскоре после последнего использования,
не вися неограниченно.

Продление — самое простое решение: **не** обновлять TTL существующего
ресурса явным вызовом, а просто дать `usableCache` вернуть `false` при
истечении, что естественно триггерит `ensureCacheAsync` пересоздать кэш
заново на следующем запросе с тем же `hash`. Явный `Caches.Update` (если
`google.golang.org/genai` его предоставляет) — оптимизация follow-up, не
обязательная для v1: пересоздание раз в ~30 минут активного использования
дёшево по сравнению с ценой, которую кэш экономит на каждом из
десятков вызовов за это время.

### 4.6 Инвалидация при смене конфигурации

Явного `Caches.Delete` при устаревании конфига/MCP-серверов не требуется
для корректности — `hashTools` на устаревший набор просто перестаёт
совпадать с `entry.hash`, `usableCache` возвращает `false`, снова уходит
в uncached-путь и асинхронно создаёт новый кэш под новый `hash`. Старый
ресурс просто больше не референсится и доживает до истечения TTL —
небольшая, самоограниченная по времени переплата за хранение, не ошибка.
Явное удаление можно добавить позже как чисто cost-side оптимизацию, не
меняющую корректность.

---

## 5. Что кэшируется, а что нет — сводка

| Часть запроса | Кэшируется? | Почему |
|---|---|---|
| `Tools` (схемы тулов) | Да, через `CachedContent`, per-API-key | Побайтово одинаковы для всех пользователей/ходов/разговоров при неизменном конфиге — единственный кандидат, стабильный **глобально**, а не только в рамках одного разговора |
| `SystemInstruction`, `stable`-часть (персона + спикер + shared/personal память, `docs/adr/system-prompt-caching.md`) | Нет, всегда инлайн | Стабильна только в рамках одного разговора одного пользователя — под явный, все-или-ничего `CachedContent` понадобился бы кэш на каждую (пользователь, разговор, ключ) тройку, а не один глобальный — see §2 |
| `SystemInstruction`, `volatile`-часть (текущее время) | Нет, всегда инлайн | Меняется на каждый ход разговора — никогда не совпадёт повторно, кэшировать бессмысленно в принципе |
| `Contents` (история сообщений хода) | Нет, всегда инлайн | По определению разное на каждой итерации — растёт по ходу цикла |

---

## 6. Что нужно перепроверить по факту в `google.golang.org/genai` на этапе реализации

Этот ADR фиксирует архитектурное решение (что кэшировать, как это
пережить с ротацией ключей, как не блокировать живой запрос), а не точные
имена методов SDK — они не проверялись здесь напрямую и могут отличаться:

- Точная сигнатура создания tools-only `CachedContent` (без `Contents`) —
  поддерживает ли `google.golang.org/genai` кэш, содержащий только `Tools`
  без `Contents`/`SystemInstruction`, или API требует непустой `Contents`.
- Можно ли в одном запросе одновременно указать `CachedContent` (для
  `Tools`) и обычный инлайн `SystemInstruction` — то есть частичное,
  смешанное использование кэша и свежих полей в одном вызове
  `GenerateContentStream`, а не «всё из кэша или всё инлайн».
- Минимальный размер контента для допуска к кэшированию (у Gemini
  исторически было пороговое значение порядка 1-2K токенов в разных
  моделях/тирах) — при 66 тулах текущий объём точно выше любого разумного
  порога, но стоит перепроверить для `gemini-3.5-flash-lite`/
  `gemini-3.6-flash` конкретно.
- Фактическая цена хранения кэша (за токен-час) для этих моделей — чтобы
  подтвердить, что 30-минутный TTL при типичной частоте вызовов
  household'а (см. `../../system-prompt.md`) даёт чистую экономию, а не
  паразитную стоимость простоя.

---

## 7. Взаимодействие с `docs/adr/lazy-mcp-tool-loading.md`

Независимые, но взаимоусиливающие изменения:

- Без ленивой загрузки: кэшируется весь нынешний статичный список из 66
  тулов — он и так не меняется от вызова к вызову, кэш почти никогда не
  промахивается по `hash`.
- С ленивой загрузкой: базовый список (built-ins + `ha_*` + один
  `load_tool_group` stub) меньше и **ещё стабильнее** — большинство
  ходов вообще не трогают `load_tool_group`, значит `hash` совпадает с
  кэшем практически всегда. На ходах, где домен реально загружается
  (`composeTools` добавляет реальные схемы одного MCP-сервера), `hash`
  на эту конкретную попытку отличается от базового — `resolveToolsForAttempt`
  корректно уходит в uncached-путь **только для этого одного вызова**, не
  ломая и не инвалидируя кэш базового набора для всех остальных ходов.
  Это ровно тот путь, для которого §4.3 и написан — рассинхрон `hash`
  всегда деградирует к «отправить инлайн», никогда к ошибке.

---

## 8. Проверка после реализации

1. Включить `context_caching: true` для `gemini-lite` в
   `../../config/llm.yaml`, перезапустить (`gemini.New` больше не должен
   отклонять старт).
2. Первый вызов после старта (или после смены конфига) — кэш ещё не
   создан, `logs/llm.log` показывает обычный полный `Tools` инлайн,
   `usage` без признаков переиспользования.
3. Второй и последующие вызовы (после того, как фоновая `ensureCacheAsync`
   успела создать кэш) — `usage` в ответе показывает ненулевой
   `cachedContentTokenCount` (или его актуальный аналог в SDK на момент
   реализации), а `promptTokenCount` заметно ниже, чем в
   `../../system-prompt.md`'s исходном снимке при сопоставимом наборе тулов.
4. Принудительно вызвать ротацию ключа (например, временно испортить
   первый `GEMINI_API_KEY_*`) — следующая попытка на другом ключе не
   падает с ошибкой «cache not found», а корректно уходит в uncached-путь
   для этого ключа и параллельно заводит для него собственный кэш.
5. Изменить конфиг MCP-серверов (добавить/выключить сервер) и
   перезапустить — старый `hash` перестаёт совпадать, первый запрос после
   рестарта снова идёт инлайн, кэш пересоздаётся под новый набор
   автоматически, без ручного вмешательства.
