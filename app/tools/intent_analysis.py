import json
import logging
import os

import requests

# --- Logging Setup ---
log = logging.getLogger(__name__)

# --- Configuration ---
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")


def _extract_json_from_string(text: str) -> str:
    """Finds and extracts the first valid JSON object from a string."""
    start_index = text.find("{")
    end_index = text.rfind("}")
    if start_index != -1 and end_index != -1 and end_index > start_index:
        return text[start_index : end_index + 1]
    log.warning(f"Could not find a valid JSON object in the model's response: {text}")
    return "{}"


def decide_if_search_is_needed(prompt: str, model: str) -> bool:
    """
    Uses a fine-tuned LLM to determine if the user's prompt requires a web search.
    This corresponds to the "Intent Analysis" step in the flowchart.
    """
    if not OLLAMA_HOST:
        log.error("OLLAMA_HOST_URL is not set. Defaulting to performing a search.")
        return True

    fine_tuned_model = "intent_analysis:latest"
    clean_json_str = "{}"

    try:
        log.info(f"Performing intent analysis for prompt: '{prompt}'")
        response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={
                "model": fine_tuned_model,
                "prompt": prompt,
                "stream": False,
                "format": "json",
                "keep_alive": "5m",
                "options": {"temperature": 0.0},
            },
            timeout=20,
        )

        response.raise_for_status()

        ollama_envelope = response.json()
        response_json_str = ollama_envelope.get("response", "{}")
        clean_json_str = _extract_json_from_string(response_json_str)

        intent_data = json.loads(clean_json_str)
        search_needed = intent_data.get("search_needed", False)

        if not isinstance(search_needed, bool):
            log.warning(
                f"Model returned a non-boolean for search_needed. Defaulting to False. Response: {search_needed}"
            )
            return False

        log.info(f"Intent analysis result: search_needed = {search_needed}")
        return search_needed

    except requests.exceptions.RequestException as e:
        log.error(
            f"Error contacting Ollama for intent analysis: {e}. Defaulting to search."
        )
        return True
    except json.JSONDecodeError:
        log.error(
            f"Failed to decode JSON from Ollama intent response: {clean_json_str}. Defaulting to search."
        )
        return True
    except Exception as e:
        log.error(
            f"Unexpected error during intent analysis: {e}. Defaulting to search.",
            exc_info=True,
        )
        return True
