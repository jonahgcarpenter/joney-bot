import logging
import os
from urllib.parse import quote_plus

import requests

# --- Logging Setup ---
log = logging.getLogger(__name__)

# --- Configuration ---
SEARXNG_URL = os.getenv("SEARXNG_URL")


def query_searxng(query: str, max_results: int = 5) -> str:
    """
    Queries the local SearXNG instance and returns a formatted string of results.

    Args:
        query: The search query from the user.
        max_results: The maximum number of search results to return.

    Returns:
        A formatted string containing the titles and content of the top search results,
        or an empty string if the search fails.
    """
    if not SEARXNG_URL:
        log.error("SEARXNG_URL is not set in the environment variables.")
        return ""

    encoded_query = quote_plus(query)

    search_url = f"{SEARXNG_URL}/search?q={encoded_query}&format=json"

    log.info(f"Querying SearXNG: {search_url}")

    try:
        response = requests.get(search_url, timeout=15)
        response.raise_for_status()

        data = response.json()
        results = data.get("results", [])

        if not results:
            log.info("SearXNG returned no results.")
            return "No search results found."

        # Format the top results into a context string
        context = []
        for i, result in enumerate(results[:max_results]):
            title = result.get("title", "No Title")
            content = result.get("content", "No Content")
            context.append(f"Result {i+1}: {title}\nContent: {content}")

        formatted_context = "\n\n".join(context)
        log.info(f"Successfully formatted {len(context)} search results.")
        return formatted_context

    except requests.exceptions.RequestException as e:
        log.error(f"Error connecting to SearXNG at {SEARXNG_URL}: {e}")
        return ""
    except Exception as e:
        log.error(
            f"An unexpected error occurred during SearXNG search: {e}", exc_info=True
        )
        return ""
