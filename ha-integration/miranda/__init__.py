"""The Miranda thin conversation client.

This integration holds no LLM logic of its own: it exists only to forward
Assist pipeline text (plus the speaker-recognized user_id and conversation_id)
to the Miranda Agent Service over HTTP and return its reply. All memory,
tool-calling, and model routing happens in the Agent Service — see
internal/httpapi in the main Miranda repo for the API it calls.
"""

from __future__ import annotations

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant

from .const import DOMAIN

PLATFORMS = ["conversation"]


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Set up Miranda from a config entry."""
    hass.data.setdefault(DOMAIN, {})[entry.entry_id] = entry
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    entry.async_on_unload(entry.add_update_listener(_async_update_listener))
    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Unload a config entry."""
    unloaded = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
    if unloaded:
        hass.data[DOMAIN].pop(entry.entry_id)
    return unloaded


async def _async_update_listener(hass: HomeAssistant, entry: ConfigEntry) -> None:
    """Reload the entry when its options (e.g. timeout) change."""
    await hass.config_entries.async_reload(entry.entry_id)
