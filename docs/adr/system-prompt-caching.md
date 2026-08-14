# Кэшируемый системный промпт: 3 блока + память на весь разговор

> Статус: реализовано (`../../internal/httpapi/agent_loop.go`,
> `../../internal/httpapi/orchestrator.go`, `../../internal/httpapi/summarize.go`,
> плюс `anthropic/anthropic.go` и `llm.go` в
> `github.com/archer-developer/miranda-llm`). Если что-то из описанного ниже
> разойдётся с кодом в будущем — верь коду, не этому файлу.

---

## 1. Проблема

`docs/adr/gemini-context-caching.md` разбирает кэширование для Gemini, но у
Anthropic (`claude`, верхняя ступень эскалации) `cache_control` **уже
реализован** — `anthropic.go`'s `toAnthropicMessages` и `buildTools` — и тем
не менее почти не давал эффекта между репликами одного разговора. Причина —
не в самом механизме, а в форме, в которой Miranda отправляла system-текст:

1. **Одна строка на всё.** `buildSystemPrompt` (было:
   `internal/httpapi/agent_loop.go`) склеивала персону → спикера → **текущее
   время** → shared-память → personal-память в одну строку, и `Handle`
   заворачивал её в **одно** `llm.Message{Role: RoleSystem}`. В
   `toAnthropicMessages` это становился **один** `TextBlockParam` —
   `cache_control` можно поставить только на границу блока, а блок один
   целиком.
2. **Время меняется почти на каждый ход.** `buildSystemPrompt` вызывался
   заново на каждое новое сообщение пользователя (`Orchestrator.Handle`),
   и `time.Now()` внутри него — на минутную точность. Раз время сидело **в
   середине** единственного блока, любое расхождение обнуляло совпадение
   для всего блока целиком — **включая shared/personal память**, которая
   шла после времени и сама по себе между репликами почти всегда не
   менялась.
3. **Память тоже читалась заново на каждый ход**, даже без явного изменения:
   `o.memory.ReadShared()`/`o.memory.Read(userID)` вызывались на каждый
   `Handle()`. Если где-то в разговоре срабатывал `remember_this`, то же
   самое расхождение возникало и без участия времени — а то, что модель
   только что запомнила, и так уже видно в истории этого же разговора как
   tool-call + результат, повторно скармливать это через system prompt
   избыточно.

В сумме: `cache_control` реально работал только **внутри одного хода**
(между итерациями одного `runAgentLoop`, пока `messages[0]` не менялось — до
`maxToolIterations = 15`), но не между репликами одного и того же
разговора — каждая новая реплика пересчитывала время → новый блок →
кэш-промах на всю систему заново, каждый раз по полной цене.

---

## 2. Решение

### 2.1 Три логических уровня волатильности, не два

- **Персона** (`AgentConfig.SystemPrompt`) — меняется только на редеплой.
- **Память** (shared + personal) — меняется только по явному `remember_this`
  или разовой summarization при закрытии сессии — редко, но не никогда.
- **Время** — меняется практически на каждый ход, кэшировать бессмысленно
  в принципе.

Реализовано как два блока (не три) — «стабильный» = персона + спикер +
память, «волатильный» = время — потому что персона и память меняются с
частотой, для которой единого триггера инвалидации достаточно: смена
персоны — редеплой (новый процесс, кэш и так пересоздаётся), смена памяти —
конец разговора (см. §2.3). Внутри одного открытого разговора оба
неизменны, значит не было смысла заводить для них разные блоки/breakpoint'ы.

### 2.2 `buildSystemPrompt` — split вместо одной строки

`internal/httpapi/agent_loop.go:439-462`:

```go
func (o *Orchestrator) buildSystemPrompt(userID, sharedMemory, userMemory string) (stable, volatile string) {
    stable = o.baseSystemPrompt
    if name := o.currentUserName(userID); name != "" {
        stable += "\n\nСейчас с тобой разговаривает: " + name + " ..."
    }
    if sharedMemory != "" {
        stable += "\n\nShared household memory:\n" + sharedMemory
    }
    if userMemory != "" {
        stable += "\n\nWhat you remember about this user:\n" + userMemory
    }

    now := time.Now().In(o.userLocation(userID))
    volatile = "Текущее время пользователя: " + now.Format("2006-01-02 15:04 MST") + "."

    return stable, volatile
}
```

`Orchestrator.Handle` (`internal/httpapi/orchestrator.go:437-449`) шлёт их
как **два** отдельных `llm.Message{Role: RoleSystem}`, стабильный первым:

```go
stableSystem, volatileSystem := o.buildSystemPrompt(userID, sharedMem, memContent)
...
messages := append([]llm.Message{
    {Role: llm.RoleSystem, Content: stableSystem},
    {Role: llm.RoleSystem, Content: volatileSystem},
}, priorMessages...)
```

`history.SetSystemPrompt` (только для отображения в web UI, никогда не
перечитывается обратно в живой цикл) по-прежнему получает одну строку —
`stableSystem + "\n\n" + volatileSystem` — расщепление касается только того,
что реально уходит в LLM.

### 2.3 Память — читается один раз на разговор, не на ход

`internal/httpapi/agent_loop.go:466-524`, новый `cachedMemory` +
`conversationMemory`/`clearConversationMemory` на `Orchestrator`:

```go
type cachedMemory struct {
    shared, personal string
}

func (o *Orchestrator) conversationMemory(userID, convID string) (shared, personal string, err error) {
    o.memoryMu.Lock()
    if cached, ok := o.memoryCache[convID]; ok {
        o.memoryMu.Unlock()
        return cached.shared, cached.personal, nil
    }
    o.memoryMu.Unlock()

    shared, err = o.memory.ReadShared()
    ...
    personal, err = o.memory.Read(userID)
    ...

    o.memoryMu.Lock()
    o.memoryCache[convID] = cachedMemory{shared: shared, personal: personal}
    o.memoryMu.Unlock()
    return shared, personal, nil
}

func (o *Orchestrator) clearConversationMemory(convID string) {
    o.memoryMu.Lock()
    delete(o.memoryCache, convID)
    o.memoryMu.Unlock()
}
```

`Orchestrator` получил `memoryMu sync.Mutex` + `memoryCache map[string]cachedMemory`
(`internal/httpapi/orchestrator.go:236-242`), инициализируется в
`NewOrchestrator`. `Handle` (`orchestrator.go:415-419`) читает через
`o.conversationMemory(userID, convID)` вместо прямых `o.memory.ReadShared()`/
`o.memory.Read(userID)`.

Эвикция — в трёх местах, покрывающих все пути закрытия разговора:

- `Handle`, ветка `control.forgetRequested`, после успешного
  `DeleteConversation` (`orchestrator.go:497`).
- `summarizeConversation`, оба return-пути — пустой разговор
  (`summarize.go:105`) и обычный, после `EndConversationWithSummary`
  (`summarize.go:146`) — общая функция для idle sweep и явного
  `end_conversation`, так что один патч покрывает оба вызывающих пути.

`internal/httpapi/summarize.go`'s собственные чтения памяти
(`o.memory.Read(userID)`/`o.memory.ReadShared()` внутри `summarizeConversation`,
строки 109-116) **намеренно не тронуты** — summarization обязана видеть
самое свежее состояние на диске, чтобы корректно смёржить новые факты, а не
дублировать то, что уже кэш-снимок мог не увидеть.

### 2.4 Anthropic: breakpoint на первый блок, не на последний

`anthropic.go`'s `toAnthropicMessages` (в `github.com/archer-developer/miranda-llm`)
раньше ставил `cache_control` на **последний** system-блок:

```go
// было
system[len(system)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
```

При одном блоке это не имело значения. При двух — с волатильным блоком,
намеренно идущим последним (§2.2) — это ставило breakpoint ровно на блок,
который гарантированно не совпадёт никогда, полностью убивая кэш для
любого вызывающего кода, перешедшего на несколько system-сообщений. Правка:

```go
// стало
system[0].CacheControl = anthropic.NewCacheControlEphemeralParam()
```

Конвенция задокументирована в трёх местах, чтобы не разойтись в будущем:
`toAnthropicMessages`'s doc comment, `llm.RoleSystem`'s doc comment
(`llm.go`) и `buildSystemPrompt`'s doc comment на стороне Miranda —
стабильный блок всегда первый, волатильный всегда последний и никогда не
маркируется.

Gemini (`gemini/gemini.go`'s `toGeminiContents`) и OpenAI-compat
(`openaicompat`'s `toOpenAIMessages`) уже устойчивы к нескольким
`RoleSystem`-сообщениям без изменений — Gemini собирает их в несколько
`Part` одного `SystemInstruction`, OpenAI-compat просто эмитит несколько
system-сообщений подряд. `docs/adr/gemini-context-caching.md` по-прежнему
не кэширует `SystemInstruction` вовсе (только `Tools`) — эта работа его не
затрагивает.

---

## 3. Что сознательно принято как компромисс

**Кросс-разговорная задержка в пределах окна простоя.** Если разговор Саши
уже открыт (до истечения `Memory.SessionIdleTimeoutMinutes`, по умолчанию
25 мин) и в это время Аня в своём параллельном разговоре вызывает
`remember_this(scope="shared")`, Сашин уже открытый разговор этот факт не
увидит вплоть до собственного переоткрытия (idle-таймаут или явный
`end_conversation`/`forget_conversation`). До этой правки — каждый `Handle()`
перечитывал файл, так что кросс-пользовательские shared-факты долетали
почти сразу. Для двух пользователей и относительно редких shared-фактов это
принятый компромисс, а не побочный эффект: см. тест
`TestOrchestrator_MemoryReadOncePerConversation`
(`internal/httpapi/system_prompt_cache_test.go`), который явно проверяет
именно это поведение (факт, записанный «извне» текущего разговора, не
просачивается в уже закэшированный snapshot).

---

## 4. Проверка

Автоматические тесты (все проходят на момент написания):

- `internal/httpapi/system_prompt_cache_test.go`:
  - `TestOrchestrator_SystemPromptSentAsTwoMessagesStableFirst` — стабильный
    блок никогда не содержит «Текущее время пользователя», волатильный
    содержит.
  - `TestOrchestrator_MemoryReadOncePerConversation` — факт, записанный на
    диск в обход текущего разговора, не появляется в system-промпте
    следующего хода того же разговора.
  - `TestOrchestrator_MemoryCacheClearedWhenConversationEnds` — эвикция
    через `SummarizeIdleSessions`.
  - `TestOrchestrator_MemoryCacheClearedWhenConversationForgotten` —
    эвикция через `forget_conversation`.
- `test/integration/agent_loop_test.go`'s
  `TestAgentLoop_ToolCallRoundTripIsReplayedOnNextTurnInSameConversation` —
  обновлён под новую форму (5 сообщений вместо 4: два system вместо
  одного).
- `miranda-llm/anthropic/anthropic_test.go`:
  - `TestToAnthropicMessages_SingleSystemBlockGetsCacheBreakpoint` —
    единственный блок по-прежнему маркируется (обратная совместимость).
  - `TestToAnthropicMessages_CachesFirstSystemBlockNotLast` — два блока,
    breakpoint на первом, не на втором.

Ручная проверка на проде (после деплоя с обновлённым `miranda-llm`): в
`logs/llm.log` для провайдера `claude` (эскалация) сравнить второй и
последующие вызовы одного разговора — `usage`/`cache_read_input_tokens` (или
эквивалентное поле в трейсе) должно показывать переиспользование, в отличие
от состояния на момент `../../system-prompt.md`'s исходного снимка.

---

## 5. Что не входит в эту правку

- Сам факт, что `default_provider: gemini-lite` не кэширует ничего из
  system-текста — см. `docs/adr/gemini-context-caching.md`, отдельная
  работа, независимая от этой.
- Обновление `../../go.mod`'а Miranda на новую ревизию `miranda-llm` после
  того, как правки в `anthropic.go`/`llm.go` закоммичены и запушены в тот
  репозиторий — вне рамок кода в этом репозитории, но необходимо, чтобы
  правка из §2.4 реально попала в собранный бинарник Miranda.
