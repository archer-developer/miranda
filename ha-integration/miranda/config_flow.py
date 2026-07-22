"""Config flow for the Miranda thin conversation client."""

from __future__ import annotations

import asyncio
import logging
from typing import Any

import aiohttp
import voluptuous as vol

from homeassistant import config_entries
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, callback
from homeassistant.data_entry_flow import FlowResult
from homeassistant.helpers.aiohttp_client import async_get_clientsession

from .const import (
    CONF_AUTH_TOKEN,
    CONF_BASE_URL,
    CONF_TIMEOUT,
    DEFAULT_BASE_URL,
    DEFAULT_NAME,
    DEFAULT_TIMEOUT,
    DOMAIN,
)

_LOGGER = logging.getLogger(__name__)

STEP_USER_SCHEMA = vol.Schema(
    {
        vol.Required(CONF_BASE_URL, default=DEFAULT_BASE_URL): str,
        vol.Optional(CONF_AUTH_TOKEN, default=""): str,
    }
)


async def _validate_connection(hass: HomeAssistant, base_url: str) -> None:
    """Hit Miranda's unauthenticated /healthz endpoint to confirm it's reachable."""
    session = async_get_clientsession(hass)
    async with asyncio.timeout(10):
        async with session.get(f"{base_url}/healthz") as resp:
            if resp.status != 200:
                raise CannotConnect(f"unexpected status {resp.status}")


class MirandaConfigFlow(config_entries.ConfigFlow, domain=DOMAIN):
    """Handle a config flow for Miranda."""

    VERSION = 1

    async def async_step_user(self, user_input: dict[str, Any] | None = None) -> FlowResult:
        errors: dict[str, str] = {}

        if user_input is not None:
            base_url = user_input[CONF_BASE_URL].rstrip("/")
            token = user_input.get(CONF_AUTH_TOKEN) or ""

            try:
                await _validate_connection(self.hass, base_url)
            except (CannotConnect, aiohttp.ClientError, TimeoutError, asyncio.TimeoutError):
                _LOGGER.warning("Could not reach Miranda at %s", base_url)
                errors["base"] = "cannot_connect"
            else:
                await self.async_set_unique_id(base_url)
                self._abort_if_unique_id_configured()
                return self.async_create_entry(
                    title=DEFAULT_NAME,
                    data={CONF_BASE_URL: base_url, CONF_AUTH_TOKEN: token},
                )

        return self.async_show_form(step_id="user", data_schema=STEP_USER_SCHEMA, errors=errors)

    @staticmethod
    @callback
    def async_get_options_flow(config_entry: ConfigEntry) -> MirandaOptionsFlow:
        return MirandaOptionsFlow(config_entry)


class MirandaOptionsFlow(config_entries.OptionsFlow):
    """Options: currently just the per-request HTTP timeout."""

    def __init__(self, config_entry: ConfigEntry) -> None:
        self._config_entry = config_entry

    async def async_step_init(self, user_input: dict[str, Any] | None = None) -> FlowResult:
        if user_input is not None:
            return self.async_create_entry(title="", data=user_input)

        schema = vol.Schema(
            {
                vol.Optional(
                    CONF_TIMEOUT,
                    default=self._config_entry.options.get(CONF_TIMEOUT, DEFAULT_TIMEOUT),
                ): vol.Coerce(int),
            }
        )
        return self.async_show_form(step_id="init", data_schema=schema)


class CannotConnect(Exception):
    """Raised when Miranda can't be reached during config flow validation."""
