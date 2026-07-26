package telegram

// Update is the subset of Telegram's Update object the webhook handler
// (internal/httpapi) needs. Every update type other than a plain text
// message (edited messages, channel posts, callback queries, ...) decodes
// with a nil Message, which the handler treats as nothing to do.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is the subset of Telegram's Message object needed to route a
// turn to the right Miranda user and reply to the right chat.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *From  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

// From identifies the Telegram user who sent a Message. Username is what
// gets matched against UserConfig.TelegramName — it's empty if the sender
// hasn't set a Telegram @username at all, in which case they can't be
// mapped to a Miranda account no matter what's configured.
type From struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

// Chat identifies where a reply must be sent — its ID is exactly what
// Client.SendMessage needs, and it's also the value ChatStore persists.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
