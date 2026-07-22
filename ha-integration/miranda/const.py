"""Constants for the Miranda thin conversation client."""

DOMAIN = "miranda"

CONF_BASE_URL = "base_url"
CONF_AUTH_TOKEN = "auth_token"
CONF_TIMEOUT = "timeout"

DEFAULT_NAME = "Miranda"
DEFAULT_BASE_URL = "http://localhost:8787"
DEFAULT_TIMEOUT = 30  # seconds; the agent loop can call an LLM + several tools per turn
