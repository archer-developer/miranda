package webui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAvatarURL(t *testing.T) {
	require.Equal(t, "", avatarURL(""))
	require.Equal(t, "https://example.com/a.png", avatarURL("https://example.com/a.png"))
	require.Equal(t, "http://example.com/a.png", avatarURL("http://example.com/a.png"))
	require.Equal(t, "/static/avatars/alex.png", avatarURL("alex.png"))
}
