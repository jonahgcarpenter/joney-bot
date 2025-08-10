import json
import logging
import os
from datetime import datetime
from urllib.parse import quote_plus

import requests

# --- Logging Setup ---
log = logging.getLogger(__name__)

# --- Configuration ---
SEARXNG_URL = os.getenv("SEARXNG_URL")
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")


def _extract_json_from_string(text: str) -> str:
    """
    Finds and extracts the first valid JSON object from a string.
    Handles cases where the model adds extra text or newlines.
    """
    start_index = text.find("{")
    end_index = text.rfind("}")

    if start_index != -1 and end_index != -1 and end_index > start_index:
        return text[start_index : end_index + 1]

    log.warning(f"Could not find a valid JSON object in the model's response: {text}")
    return "{}"


def _generate_search_queries(prompt: str, model: str) -> list[str]:
    """
    Uses an LLM to analyze the user's prompt and generate a list of effective search queries.
    """
    if not OLLAMA_HOST:
        log.error(
            "OLLAMA_HOST_URL is not set. Cannot generate intelligent search query. Falling back to direct search."
        )
        return [prompt]

    current_date = datetime.now().strftime("%Y-%m-%d")

    system_prompt = (
        f"You are a highly specialized Search Query Generator. Your entire purpose is to create search queries that will arm a downstream, unhinged AI with the information it needs to be as funny and insulting as possible. The current date is {current_date}.\n\n"
        "Follow this logic precisely:\n\n"
        "1.  **Analyze the User's Prompt.** First, determine if it's a straightforward, factual question OR if it's conversational/joking/insulting.\n\n"
        "2.  **If it IS a Factual Question:** (e.g., 'What is the capital of Canada?')\n"
        "    - Generate 1-3 direct search queries to find the answer.\n\n"
        "3.  **If it is NOT a Factual Question:** (e.g., insults, jokes, questions with slang)\n"
        "    - Your job immediately becomes **Slang and Weirdness Detection.**\n"
        "    - Scan the prompt for ANY words or phrases that are slang, made-up, or unusual. Your goal is to define these terms.\n"
        "    - For 'i have no bitches, how can i aquire more of the huzz', the terms 'bitches' and 'huzz' are slang. You MUST generate definitional queries like `[\"huzz meaning\", \"bitches slang definition\"]`.\n"
        "    - If a non-factual prompt contains ONLY common words (e.g., 'shut up you are dumb'), then and ONLY then should you return an empty list because there is nothing to define.\n\n"
        "## Final Output:\n"
        "You MUST respond with ONLY a JSON object with a single key: 'search_queries'. The value will be a list of strings. This list can be empty, but only if the rules above are met."
    )

    full_prompt = f'{system_prompt}\n\nUser Prompt: "{prompt}"'
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
                "keep_alive": 0,
                "options": {"temperature": 0.0},
            },
            timeout=25,
        )
        response.raise_for_status()

        ollama_envelope = response.json()
        log.info(f"Ollama raw envelope: {ollama_envelope}")
        response_json_str = ollama_envelope.get("response", "{}")
        clean_json_str = _extract_json_from_string(response_json_str)
        inner_data = json.loads(clean_json_str)

        search_queries = inner_data.get("search_queries", [])

        if not isinstance(search_queries, list):
            log.warning(
                f"Model returned a non-list for search_queries. Using fallback. Response: {search_queries}"
            )
            return [prompt]

        if search_queries:
            log.info(
                f"Generated {len(search_queries)} search queries: {search_queries}"
            )
        else:
            log.info("LLM decided no search is necessary for this prompt.")

        return search_queries

    except requests.exceptions.RequestException as e:
        log.error(f"Error contacting Ollama to generate search queries: {e}")
        return [prompt]
    except json.JSONDecodeError:
        log.error(
            f"Failed to decode JSON from cleaned Ollama response content: {clean_json_str}"
        )
        return [prompt]
    except Exception as e:
        log.error(
            f"An unexpected error occurred during query generation: {e}", exc_info=True
        )
        return [prompt]


def query_searxng(query: str, max_results: int = 3) -> str:
    """
    Queries the local SearXNG instance and returns a formatted string of results.
    Now fetching fewer results per query since we run multiple queries.
    """
    if not SEARXNG_URL:
        log.error("SEARXNG_URL is not set in the environment variables.")
        return ""

    encoded_query = quote_plus(query)
    search_url = f"{SEARXNG_URL}/search?q={encoded_query}&format=json"
    log.info(f"Querying SearXNG for: '{query}'")

    try:
        response = requests.get(search_url, timeout=15)
        response.raise_for_status()
        data = response.json()
        results = data.get("results", [])

        if not results:
            log.info(f"No results found for query: '{query}'")
            return ""

        context = []
        for result in results[:max_results]:
            title = result.get("title", "No Title")
            content = result.get("content", "No Content")
            context.append(f"Title: {title}\nContent: {content}")

        return "\n\n".join(context)

    except requests.exceptions.RequestException as e:
        log.error(f"Error connecting to SearXNG at {SEARXNG_URL}: {e}")
        return ""
    except Exception as e:
        log.error(
            f"An unexpected error during SearXNG search for '{query}': {e}",
            exc_info=True,
        )
        return ""


def think_and_search(prompt: str, model: str) -> str:
    """
    Orchestrates the intelligent search process. It generates multiple queries,
    executes a search for each, and combines the results.
    """
    search_queries = _generate_search_queries(prompt, model)

    if not search_queries:
        return ""

    all_results_context = []
    seen_content = set()

    for query in search_queries:
        if not query.strip():
            continue

        query_results = query_searxng(query)
        if query_results:
            if query_results not in seen_content:
                all_results_context.append(query_results)
                seen_content.add(query_results)

    if not all_results_context:
        log.warning("All search queries returned no results.")
        return ""

    final_context = "\n\n---\n\n".join(all_results_context)
    log.info(
        f"Successfully combined results from {len(all_results_context)} search queries."
    )
    return final_context
