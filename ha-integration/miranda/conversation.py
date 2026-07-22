"""The Miranda conversation entity: a thin forwarder to the Agent Service.

No LLM logic, memory, or tool-calling lives here — this entity's only job is
to package up (source, user_id, text, conversation_id) and POST it to
Miranda's unified command interface (POST /api/v1/input), then relay the
reply back into the Assist pipeline. See docs/PROJECT_PREREQUISITES.md in the
main repo for why: HA is one input/output channel among several, not where
the agent's brain lives.
"""

from __future__ import annotations

import asyncio
import logging

import aiohttp

from homeassistant.components import conversation
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers import intent
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import CONF_AUTH_TOKEN, CONF_BASE_URL, CONF_TIMEOUT, DEFAULT_TIMEOUT, DOMAIN

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    """Set up the Miranda conversation entity for this config entry."""
    async_add_entities([MirandaConversationEntity(entry)])


class MirandaConversationEntity(conversation.ConversationEntity):
    """Forwards Assist pipeline text to the Miranda Agent Service."""

    _attr_has_entity_name = True
    _attr_name = None  # use the config entry's title ("Miranda") as the name

    def __init__(self, entry: ConfigEntry) -> None:
        self._entry = entry
        self._attr_unique_id = entry.entry_id

    @property
    def supported_languages(self) -> list[str] | str:
        # Miranda / the underlying LLM decides how to handle language, not HA.
        return "*"

    async def async_process(
        self, user_input: conversation.ConversationInput
    ) -> conversation.ConversationResult:
        base_url = self._entry.data[CONF_BASE_URL]
        token = self._entry.data.get(CONF_AUTH_TOKEN) or ""
        timeout = self._entry.options.get(CONF_TIMEOUT, DEFAULT_TIMEOUT)

        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"

        # context.user_id is populated by the speaker-recognition component
        # upstream in the pipeline; it's absent for e.g. the dev-tools "Try"
        # box, where we fall back to a generic id.
        user_id = user_input.context.user_id if user_input.context else None

        payload = {
            "source": "ha_assist",
            "user_id": user_id or "unknown",
            "text": user_input.text,
            "conversation_id": user_input.conversation_id or "",
        }

        session = async_get_clientsession(self.hass)
        try:
            async with asyncio.timeout(timeout):
                async with session.post(
                    f"{base_url}/api/v1/input", json=payload, headers=headers
                ) as resp:
                    if resp.status == 401:
                        raise MirandaError("unauthorized — check the configured auth token")
                    if resp.status != 200:
                        body = await resp.text()
                        raise MirandaError(f"status {resp.status}: {body}")
                    data = await resp.json()
        except (aiohttp.ClientError, TimeoutError, asyncio.TimeoutError, MirandaError) as err:
            _LOGGER.error("Miranda request failed: %s", err)
            return _error_result(user_input, "Не удалось связаться с Miranda.")

        response = intent.IntentResponse(language=user_input.language)
        response.async_set_speech(data.get("reply", ""))
        return conversation.ConversationResult(
            response=response,
            conversation_id=data.get("conversation_id") or user_input.conversation_id,
        )


def _error_result(
    user_input: conversation.ConversationInput, message: str
) -> conversation.ConversationResult:
    response = intent.IntentResponse(language=user_input.language)
    response.async_set_error(intent.IntentResponseErrorCode.UNKNOWN, message)
    return conversation.ConversationResult(
        response=response, conversation_id=user_input.conversation_id
    )


class MirandaError(Exception):
    """Raised for any non-200 response from the Agent Service."""
