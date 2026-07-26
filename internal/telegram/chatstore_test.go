package telegram

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatStore_SaveAndGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats.json")
	s, err := OpenChatStore(path)
	require.NoError(t, err)

	_, ok := s.Get("alex")
	require.False(t, ok)

	require.NoError(t, s.Save("alex", 555))
	id, ok := s.Get("alex")
	require.True(t, ok)
	require.Equal(t, int64(555), id)
}

func TestChatStore_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats.json")
	s, err := OpenChatStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Save("anna", 42))

	reopened, err := OpenChatStore(path)
	require.NoError(t, err)
	id, ok := reopened.Get("anna")
	require.True(t, ok)
	require.Equal(t, int64(42), id)
}

func TestChatStore_MissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "chats.json")
	s, err := OpenChatStore(path)
	require.NoError(t, err)
	_, ok := s.Get("nobody")
	require.False(t, ok)
}

func TestChatStore_SaveUpdatesExistingUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats.json")
	s, err := OpenChatStore(path)
	require.NoError(t, err)

	require.NoError(t, s.Save("alex", 1))
	require.NoError(t, s.Save("alex", 2))

	id, ok := s.Get("alex")
	require.True(t, ok)
	require.Equal(t, int64(2), id)
}
