# Нативный канал уведомлений (`internal/notify`) вместо авто-CC в Telegram

> Статус: реализовано (2026-09-28). Ссылки на строки ниже соответствуют
> состоянию репозитория на момент написания документа и могли разойтись с
> кодом с тех пор — верь коду, не этому файлу.

## 1. Проблема

До этого изменения у Miranda был единственный канал для проактивных
уведомлений — Telegram (`internal/telegram`), и он использовался
неявно/принудительно сразу в двух местах:

- `deliverReminder` (`internal/agent_loop/schedule.go`) всегда
  дополнительно, best-effort, слал в Telegram **каждое** сработавшее
  напоминание — независимо от того, где оно было создано, лишь бы
  `o.telegram != nil`. Отключить это можно было только полностью выключив
  Telegram (`telegram.enabled: false`), что заодно убивало и явный
  `send_telegram`.
- Напоминание, созданное из веб-UI (или из неизвестного источника),
  доставлялось молча дописыванием в историю диалога
  (`appendReminderToHistory`) — увидеть его можно было только открыв
  вкладку в момент срабатывания или зайдя в историю позже; никакого
  персистентного, отдельно просматриваемого списка «что мне сообщила
  Miranda не в ответ на вопрос» не было вообще.
- Модель не имела способа **проактивно** уведомить конкретного члена
  семьи в произвольный момент, кроме `send_telegram` — то есть любой
  запрос вида «отправь Ане уведомление, что...» синонимично трактовался
  как просьба использовать Telegram, даже когда пользователь имел в виду
  общий смысл «сообщи ей».

Отдельно PWA-обёртка (`internal/webui/templates/manifest.webmanifest`,
`static/js/{sw.js,pwa.js}`) уже была на месте, но `sw.js` был чисто
установочной заглушкой без обработки `push`/`notificationclick` — то есть
инфраструктура для настоящих OS-уведомлений (даже при закрытой
вкладке/приложении) не использовалась вовсе.

## 2. Решение

### 2.1 `internal/notify` — фид уведомлений + Web Push как один пакет

Один новый пакет владеет и персистентным фидом (то, что видно под
колокольчиком), и опциональной доставкой через браузерный Web Push
(RFC 8030/8291/8292) — в отличие от Telegram, здесь нет отдельного
входящего вебхука, который стоило бы изолировать, так что это одна
фича, а не две независимые инфраструктуры. Собственный SQLite-файл
(`Storage.NotifySQLitePath`, изоляция та же, что у
`WebAuthnSQLitePath`/`ScheduleSQLitePath`) с двумя таблицами:

- `notifications` — `id, user_id, title, body, source, created_at,
  read_at` (`source` — `"reminder"` или `"tool"`, чисто информационно).
- `webpush_subscriptions` — `endpoint PRIMARY KEY, user_id, p256dh, auth,
  user_agent, created_at`; ключ по `endpoint` (глобально уникален по
  спецификации Push API), поэтому повторная подписка того же браузера —
  обычный upsert, а не дубль строки.

`notify.Service.Notify(ctx, userID, title, body, source)` — единственная
точка входа: пишет строку в фид (всегда, пока `NotifyConfig.Enabled`), и,
если сконфигурирован Web Push (`webpush != nil`), best-effort шлёт push на
каждую подписку пользователя через
`github.com/SherClockHolmes/webpush-go` (MIT, чистый Go, оба его
транзитивных зависимости — `golang-jwt/jwt/v5` и `golang.org/x/crypto` —
уже были в дереве Miranda как индиректные). Ответ `404`/`410` от push-
сервиса (по спеку — «эндпоинт мёртв») удаляет эту подписку, чтобы не
ретраить бесконечно — та же логика, что уже была у `deliverReminder` для
детерминированных ошибок Telegram-доставки. `internal/notify` ничего не
знает про `internal/hub`/`internal/agent_loop` — та же изоляция, что у
`internal/telegram` — поэтому публикацией live-события занимается вызывающая
сторона (`Orchestrator.notifyUser`, см. 2.4).

### 2.2 Конфигурация

`NotifyConfig` (root `Config.Notify`) — включена по умолчанию (`Enabled:
true`), в отличие от `WebAuthn`/`Telegram`: сам фид не требует
деплой-специфичного секрета (та же логика, что у `ScheduleConfig`).
Вложенный `WebPush WebPushConfig` — отдельный, **выключенный по
умолчанию** флаг, поскольку VAPID-ключи специфичны для конкретного
деплоя и не могут быть безопасно сгенерированы автоматически при
старте (несохранённый ключ осиротил бы все существующие подписки при
рестарте). Ключевая пара генерируется один раз через новую подкоманду
`go run ./cmd/miranda vapid-keys` (тот же `os.Args[1]`-диспатч, что у
`backup`/`llm-trace`); публичный ключ идёт в `config.yaml`
(`notify.web_push.vapid_public_key`), приватный — в переменную окружения
`WEBPUSH_VAPID_PRIVATE_KEY` (никогда не в yaml — та же конвенция, что у
`TELEGRAM_BOT_TOKEN`).

### 2.3 Правило доставки напоминания (`deliverReminder`)

Ветка выбора канала по происхождению не изменилась структурно — изменился
только `default`-случай и судьба «всегда дополнительно» блока:

```go
switch {
case voiceOrigin:
    originErr = o.speakTextChecked(ctx, voiceText(task.Prompt))
case telegramOrigin:
    originErr = o.sendTelegramReminder(ctx, task)
default: // web UI or any other/unknown origin
    originErr = o.notifyUser(ctx, task.UserID, "Миранда", task.Prompt, "reminder")
}
```

Блок «всегда дополнительно Telegram» **удалён целиком** — это и есть
собственно поведенческое изменение, которое просил пользователь: авто-
оповещение больше не CC'ится в Telegram просто по факту его
включённости. На его месте — блок «всегда дополнительно Notify»,
гарантирующий, что каждое напоминание попадёт в фид ровно один раз
независимо от происхождения (голосовые/telegram-напоминания не проходят
через `default`-ветку выше, значит без этого блока они вообще не попали
бы в фид):

```go
if !defaultOrigin && o.notify != nil {
    if err := o.notifyUser(ctx, task.UserID, "Миранда", task.Prompt, "reminder"); err != nil {
        logger.Warn("reminder notify delivery (always-on) failed", ...)
    }
}
```

`sendTelegramReminder` и telegram-ветка не тронуты — если напоминание
реально создано из Telegram-разговора, ответ туда же остаётся логичным
(это не слепой CC, а «ответ в тот канал, откуда пришли»).

### 2.4 Инструменты модели: явное разделение `send_notification` / `send_telegram`

Новый тул `send_notification` (`internal/agent_loop/tool_catalog.go`,
`tool_dispatch.go`) зеркалит `send_telegram` один в один по форме
аргументов (`text` + опциональный `recipient`, тот же
`resolveRecipient` — общий хелпер, вынесенный из ранее продублированного
кода обоих обработчиков) и вызывает `Orchestrator.notifyUser`. Описания
обоих тулов переписаны так, чтобы явно развести их зоны ответственности:

- `send_notification`: «...default channel for any proactive,
  out-of-band message ... Use this unless the user explicitly names
  Telegram specifically».
- `send_telegram`: «...use ONLY when the user explicitly names Telegram
  itself ... not for a generic "send to my phone" request — that's
  send_notification's job».

`Orchestrator.notifyUser(ctx, userID, title, body, source)` — общая точка
входа, которой пользуются и `send_notification`, и `deliverReminder`:
вызывает `notify.Service.Notify`, затем публикует
`hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type:
"notification", Notification: &n}}` — именно здесь `internal/notify`
узнаёт о хабе, а не в самом пакете (см. 2.1).

### 2.5 Живое обновление колокольчика — переиспользование чат-WS, не новый канал

`ChatEvent` (`internal/agent_loop/orchestrator.go`) получил новый вариант
`Type: "notification"` с полем `Notification *notify.Notification` —
поверх уже существующего `GET /ws/chat/{username}`
(`internal/httpapi.handleWSChat`), который и так стримит `ChatEvent` для
залогиненного пользователя. Отдельного WS/поллинга не потребовалось.

На клиенте (`static/js/app.js`) один слушатель `chatWs.on(...)` реализует
политику:

1. Пришло `type: "notification"` → точка на колокольчике загорается
   **всегда**, независимо от текущего экрана.
2. Если в этот момент открыт именно `#/notifications` — список
   перезагружается (`notifications.refreshAndMarkRead()`), новая запись
   оказывается первой (сортировка `created_at DESC` уже на сервере, без
   клиентской пересортировки).
3. Гашение точки происходит **внутри** `refreshAndMarkRead()`, сразу
   после успешного `POST /api/notifications/read` — то есть ровно тогда,
   когда список реально отрисован и пользователь факт-но мог его увидеть,
   а не в момент получения WS-события. Та же функция вызывается и при
   обычном `mount()` экрана уведомлений — один код пути для «открыл
   список» и «список уже открыт и пришло новое», что и было явным
   требованием при уточнении задачи.

## 3. Компромиссы и открытые ограничения

- **iOS Safari** поддерживает Web Push только при установке PWA на
  домашний экран (iOS 16.4+); обычная открытая вкладка Safari push не
  получит — платформенное ограничение, не то, что можно обойти кодом.
- **Нет управления отдельными подписками** («на этом телефоне», «на этом
  ноутбуке») — один тумблер включить/выключить на профиле
  (`screens/profile.js`), покрывающий типовой случай одной подписки на
  устройство. `notify.Store.SubscriptionsForUser` уже возвращает список,
  так что список-UI можно добавить позже без миграции схемы.
- **Точка, а не число** — бейдж колокольчика показывает факт наличия
  непрочитанных, не их количество, как и просили изначально.
- **Два реальных бага найдены только при живой проверке в браузере**, не
  юнит-тестами: (1) `notify.Notification`/`Subscription` изначально были
  без JSON-тегов — сервер отдавал `Body`/`CreatedAt` вместо
  `body`/`created_at`, из-за чего `renderInlineText` падал на
  `undefined.matchAll`; (2) `notify-badge.js` красил иконку через
  `bell.innerHTML = icon(...)`, что стирало дочерний `#notify-bell-dot`
  вместе с ним — индикатор технически включался, но на уже отсоединённом
  от DOM узле. Оба класса ошибок (сериализация полей, `innerHTML`
  затирает соседний узел) стоит держать в голове при следующей похожей
  фиче — юнит-тесты на Go-структуры и на JS-модули по отдельности их не
  ловят, только сквозная проверка в реальном браузере.

## 4. Проверка после реализации

1. `go build ./...`, `go test ./...` — зелено, включая новые пакетные
   тесты `internal/notify` (feed CRUD, push-рассылка с фейковым
   `webpush.HTTPClient`, пруннинг на 404/410) и функциональные тесты
   `internal/webui` на все новые `/api/notifications*`/`/api/push/*`
   маршруты (авторизация, скоуп по текущему пользователю, 404 при
   выключенном сервисе).
2. Ручная проверка в браузере (локальный запуск с реальным
   `config/`/`data/`, `notify.enabled: true` по умолчанию,
   `web_push.enabled: false`): логин → колокольчик виден в шапке → клик
   открывает `#/notifications` с пустым состоянием → сообщение боту
   «отправь мне нотификацию с текстом ...» создаёт строку через
   `send_notification`, точка на колокольчике загорается **живьём** (без
   перезагрузки страницы) пока пользователь остаётся на экране чата →
   переход на `#/notifications` показывает новую запись первой и гасит
   точку.
3. `go run ./cmd/miranda vapid-keys` печатает валидную VAPID-пару.
4. Профиль без `web_push.enabled` не показывает секцию «Push
   notifications» — рендерится только при
   `window.MIRANDA_PUSH_ENABLED && push.isSupported()`.
