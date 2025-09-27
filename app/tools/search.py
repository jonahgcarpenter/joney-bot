import json
import logging
import os
from urllib.parse import quote_plus

import requests
from tools.system_prompts import get_search_query_generator_prompt

# --- Logging Setup ---
log = logging.getLogger(__name__)

# --- Configuration ---
SEARXNG_URL = os.getenv("SEARXNG_URL")
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")


def _extract_json_from_string(text: str) -> str:
    """
    Finds and extracts the first valid JSON object from a string.
    """
    start_index = text.find("{")
    end_index = text.rfind("}")

    if start_index != -1 and end_index != -1 and end_index > start_index:
        return text[start_index : end_index + 1]

    log.warning(f"Could not find a valid JSON object in the model's response: {text}")
    return "{}"


def _generate_search_queries(prompt: str, model: str) -> list[str]:
    """
    Uses an LLM to generate effective search queries.
    """
    if not OLLAMA_HOST:
        log.error("OLLAMA_HOST_URL is not set. Falling back to direct search.")
        return [prompt]

    system_prompt = get_search_query_generator_prompt(user_prompt=prompt)
    full_prompt = f'{system_prompt}\n\nUser Prompt: "{prompt}"'
    log.debug(f"Full prompt for search query generation:\n{full_prompt}")
    clean_json_str = "{}"

    try:
        log.info(f"Generating search queries for prompt: '{prompt}'")
        response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={
                "model": model,
                "prompt": full_prompt,
                "stream": False,
                "format": "json",
                "keep_alive": "5m",
                "options": {"temperature": 0.0},
            },
            timeout=60,
        )
        response.raise_for_status()

        ollama_envelope = response.json()
        log.debug(f"Raw Ollama search query response: {ollama_envelope}")
        response_json_str = ollama_envelope.get("response", "{}")
        clean_json_str = _extract_json_from_string(response_json_str)
        inner_data = json.loads(clean_json_str)
        search_queries = inner_data.get("search_queries", [])

        if not isinstance(search_queries, list):
            log.warning(
                f"Model returned non-list for search_queries. Using fallback. Response: {search_queries}"
            )
            return [prompt]
        if search_queries:
            log.info(
                f"Generated {len(search_queries)} search queries: {search_queries}"
            )
        else:
            log.info("LLM decided no search is necessary.")
        return search_queries

    except requests.exceptions.RequestException as e:
        log.error(f"Error contacting Ollama to generate search queries: {e}")
        return [prompt]
    except json.JSONDecodeError:
        log.error(f"Failed to decode JSON from Ollama response: {clean_json_str}")
        return [prompt]
    except Exception as e:
        log.error(f"Unexpected error during query generation: {e}", exc_info=True)
        return [prompt]


def query_searxng(query: str, max_results: int = 3) -> str:
    """
    Queries the local SearXNG instance and returns a formatted string of results.
    """
    if not SEARXNG_URL:
        log.error("SEARXNG_URL is not set in environment variables.")
        return ""

    encoded_query = quote_plus(query)
    search_url = f"{SEARXNG_URL}/search?q={encoded_query}&format=json"
    log.info(f"Querying SearXNG for: '{query}'")
    # This is the corrected line - it now logs the relevant URL
    log.debug(f"Executing search URL: {search_url}")

    try:
        response = requests.get(search_url, timeout=15)
        response.raise_for_status()
        data = response.json()
        log.debug(f"Received {len(data.get('results', []))} results from SearXNG.")
        results = data.get("results", [])
        if not results:
            log.info(f"No results found for query: '{query}'")
            return ""
        context = [
            f"Title: {r.get('title', 'N/A')}\nContent: {r.get('content', 'N/A')}"
            for r in results[:max_results]
        ]
        return "\n\n".join(context)
    except requests.exceptions.RequestException as e:
        log.error(f"Error connecting to SearXNG at {SEARXNG_URL}: {e}")
        return ""
    except Exception as e:
        log.error(
            f"Unexpected error during SearXNG search for '{query}': {e}", exc_info=True
        )
        return ""


def think_and_search(prompt: str, model: str) -> tuple[str | None, list[str]]:
    """
    Orchestrates the intelligent search process.
    """
    search_queries = _generate_search_queries(prompt, model)
    if not search_queries:
        return None, search_queries

    all_results_context = []
    seen_content = set()
    for query in search_queries:
        if not query.strip():
            continue
        query_results = query_searxng(query)
        if query_results and query_results not in seen_content:
            all_results_context.append(query_results)
            seen_content.add(query_results)

    if not all_results_context:
        log.warning("All search queries returned no results.")
        return "", search_queries

    final_context = "\n\n---\n\n".join(all_results_context)
    log.info(
        f"Successfully combined results from {len(all_results_context)} search queries."
    )
    return final_context, search_queries
