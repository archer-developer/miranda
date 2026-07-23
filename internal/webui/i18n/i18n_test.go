package i18n

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStrings_FallsBackToDefaultForUnknownLanguage(t *testing.T) {
	require.Equal(t, Strings(Default), Strings("fr"))
}

func TestIsSupported(t *testing.T) {
	for _, lang := range Languages {
		require.True(t, IsSupported(lang))
	}
	require.False(t, IsSupported("fr"))
}

func TestAllLanguages_HaveTheSameKeys(t *testing.T) {
	reference := Strings(Default)
	for _, lang := range Languages {
		strings := Strings(lang)
		require.Len(t, strings, len(reference), "locale %q has a different number of keys than %q", lang, Default)
		for key := range reference {
			_, ok := strings[key]
			require.True(t, ok, "locale %q is missing key %q", lang, key)
		}
	}
}
