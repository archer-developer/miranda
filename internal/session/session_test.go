package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateAndValidate(t *testing.T) {
	s := NewStore(time.Hour)

	token, err := s.Create("alex")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	username, ok := s.Validate(token)
	require.True(t, ok)
	require.Equal(t, "alex", username)
}

func TestValidate_UnknownTokenFails(t *testing.T) {
	s := NewStore(time.Hour)
	_, ok := s.Validate("does-not-exist")
	require.False(t, ok)
}

func TestValidate_EmptyTokenFails(t *testing.T) {
	s := NewStore(time.Hour)
	_, ok := s.Validate("")
	require.False(t, ok)
}

func TestValidate_ExpiredSessionFails(t *testing.T) {
	s := NewStore(-time.Second) // already expired the instant it's created
	token, err := s.Create("alex")
	require.NoError(t, err)

	_, ok := s.Validate(token)
	require.False(t, ok)
}

func TestDestroy_InvalidatesSession(t *testing.T) {
	s := NewStore(time.Hour)
	token, err := s.Create("alex")
	require.NoError(t, err)

	s.Destroy(token)

	_, ok := s.Validate(token)
	require.False(t, ok)
}

func TestCreate_TokensAreUnique(t *testing.T) {
	s := NewStore(time.Hour)
	t1, err := s.Create("alex")
	require.NoError(t, err)
	t2, err := s.Create("alex")
	require.NoError(t, err)
	require.NotEqual(t, t1, t2)
}
