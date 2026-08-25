# Miranda — feature source sheet (for promo material generation)

This is not the project README. It's a plain, benefit-first list of what
Miranda does, written for turning into ads/landing copy/social posts — not
for someone about to install it. No config, no YAML, no architecture.

Feed this whole file to a copywriting pass; each section below is a
self-contained unit (one feature, why it matters, how someone actually
talks to it) that a generator can lift on its own.

## One-line pitch

Miranda is a personal assistant you talk to like a person — controls your
smart home, remembers things about your life, keeps a diary, tracks what you
eat, checks your calendar, looks things up online, and reminds you of
things — all through one voice or chat, on your own hardware.

## What makes it different (the 3 things to lead with)

1. **It's one assistant everywhere, not a bot per app.** Talk to it through
   your smart speaker, a Telegram chat, or a web page — it's the same
   conversation, the same memory, picking up where you left off no matter
   which one you used last.
2. **It remembers you, on your terms.** It recalls facts across
   conversations and can resume an old conversation on request — but pin
   codes, passwords, and card numbers are automatically blanked out before
   anything touches disk.
3. **It runs on your own hardware, under your own roof.** One binary, no
   cloud subscription required for the assistant itself, no vendor lock-in
   to a single AI provider.

## Contents

- [Talk to it anywhere](#talk-to-it-anywhere)
- [Controls your smart home](#controls-your-smart-home)
- [Speaks out loud](#speaks-out-loud)
- [Remembers things about you](#remembers-things-about-you)
- [Picks up an old conversation](#picks-up-an-old-conversation)
- [Keeps a diary](#keeps-a-diary)
- [Tracks what you eat](#tracks-what-you-eat)
- [Sets reminders and routines](#sets-reminders-and-routines)
- [Manages your calendar](#manages-your-calendar)
- [Looks things up online](#looks-things-up-online)
- [Crunches numbers and data](#crunches-numbers-and-data)
- [Texts your household on Telegram](#texts-your-household-on-telegram)
- [Keeps secrets out of the logs](#keeps-secrets-out-of-the-logs)
- [Picks the right brain for the job](#picks-the-right-brain-for-the-job)
- [Any AI model you want](#any-ai-model-you-want)
- [Who it's for](#who-its-for)

---

### Talk to it anywhere

One conversation follows you across every device — say something to the
smart speaker in the kitchen, continue it from your phone over Telegram, or
open the web dashboard later. Miranda doesn't ask "which app" — it already
knows who's talking.

> *"Remind me on the fridge speaker, then check on Telegram later — same
> conversation, no repeating yourself."*

### Controls your smart home

Turns lights on, checks a thermostat, runs a scene — anything already
wired into Home Assistant, Miranda can operate by voice or text, the same
way you'd ask a person to do it.

> "Выключи свет в гостиной и включи фильм на телевизоре."

### Speaks out loud

Replies through your smart speaker in a natural voice, not a beep. Built to
work with Yandex Station hardware you already own — no extra speaker to buy.

> "Miranda, what's the weather?" → she answers out loud, right there in the
> kitchen.

### Remembers things about you

Tell it something once and it stays remembered — a preference, a fact about
your family, a recurring detail — brought back into every future
conversation without you repeating yourself.

> "Запомни: у сына аллергия на орехи." — from then on, it just knows.

### Picks up an old conversation

"Let's get back to where we left off" isn't a dead end — Miranda can search
its own conversation history and resume a specific past conversation on
request.

> "Давай вернёмся к тому разговору про отпуск на прошлой неделе."

### Keeps a diary

A private, spoken or typed journal — say what happened today and it's
saved, searchable later, without opening a separate app.

> "Запиши в дневник: сегодня прошла отличная встреча с клиентом."

### Tracks what you eat

Log meals and calories by just describing what you ate — no manual food
database lookups, no separate app to open mid-meal.

> "Я съел овсянку с бананом на завтрак — запиши."

### Sets reminders and routines

One-off reminders and recurring routines, both set up by just describing
them in plain language — no forms, no cron syntax to learn.

> "Сегодня в 22:00 напомни мне выпить таблетку — пришли на телефон."
>
> "Каждое утро в 9 голосом пожелай доброго утра и зачитай курс биткоина."

### Manages your calendar

Checks free/busy time, lists what's coming up, and creates or moves events
on your Google Calendar — by asking, not by opening the app.

> "Что у меня завтра в календаре? Добавь встречу с врачом на четверг в 15:00."

### Looks things up online

When it doesn't know something, it searches the web and reads pages for
you — live answers, not a knowledge cutoff.

> "Сколько стоит новый iPhone в России сейчас?"

### Crunches numbers and data

Hands harder tasks — real calculations, data analysis, running actual
code — off to a sandboxed execution environment and brings back the
answer, not just a guess.

> "Посчитай, сколько я потратил на такси за этот месяц по чеку, который я
> тебе прислал."

### Texts your household on Telegram

Every household member gets their own Telegram thread with Miranda — ask
it to message someone directly, or just chat with it from your phone
wherever you are.

> "Отправь мужу список покупок в Телеграм."

### Keeps secrets out of the logs

PIN codes, passwords, card numbers — anything that looks like a secret is
automatically blanked out before it's ever written to disk. It can act on a
secret the moment you say it, but never remembers the value afterward.

> You say a PIN once to unlock something; it's masked in every log and
> history entry from that point on.

### Picks the right brain for the job

Simple requests get a fast, cheap answer. A genuinely hard question gets
automatically handed up to a stronger model mid-conversation — you never
have to ask for "the smart one."

### Any AI model you want

Not locked to one AI company. Works with Claude, Gemini, or any
OpenAI-compatible model (including ones you run yourself) — swap providers
without switching assistants, with automatic fallback if one is down.

### Who it's for

Anyone who wants a single, always-listening household assistant that
remembers context across every device it's on, controls the smart home they
already have, and runs on hardware they control — not a rotating cast of
single-purpose apps and cloud dashboards.
