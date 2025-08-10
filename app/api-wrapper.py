import logging
import os
import re

from dotenv import load_dotenv

load_dotenv()

import requests
import search
import vector_db
from fastapi import BackgroundTasks, FastAPI, HTTPException, Request
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

# --- Logging Setup ---
logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)

logging.getLogger("sentence_transformers").setLevel(logging.WARNING)
logging.getLogger("transformers.modeling_utils").setLevel(logging.ERROR)

log = logging.getLogger(__name__)


# --- Configuration ---
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")
SEARXNG_URL = os.getenv("SEARXNG_URL")

# --- Initialize Model for Embeddings ---
log.info("Loading sentence transformer model...")
embedding_model = SentenceTransformer(
    "nomic-ai/nomic-embed-text-v1.5", trust_remote_code=True
)
log.info("Sentence transformer model loaded")

app = FastAPI()


# --- Pydantic Model for Input Validation ---
class PromptRequest(BaseModel):
    prompt: str
    username: str
    model: str = "joney-bot:latest"


# --- Input Sanitization Function ---
def sanitize_input(prompt: str) -> str:
    """A simple sanitizer to remove potentially harmful characters."""
    sanitized = re.sub(r"[^a-zA-Z0-9\s.,!?-]", "", prompt)
    return sanitized.strip()


# --- Background Task for Saving to DB ---
def save_to_db_background(username: str, prompt: str, response: str):
    """Generates embeddings and saves the chat log to the database."""
    prompt_embedding = embedding_model.encode(prompt)
    response_embedding = embedding_model.encode(response)
    vector_db.save_chat(
        username, prompt, response, prompt_embedding, response_embedding
    )


# --- API Endpoint ---
@app.post("/generate")
async def generate_prompt(
    request: Request, data: PromptRequest, background_tasks: BackgroundTasks
):
    """
    Receives a prompt, ALWAYS performs a web search, gets a response from Ollama
    based on the search results, and saves the original exchange to the DB.
    """
    sanitized_prompt = sanitize_input(data.prompt)

    if not sanitized_prompt:
        raise HTTPException(
            status_code=400, detail="Prompt is empty after sanitization."
        )

    try:
        # --- ALWAYS PERFORM WEB SEARCH ---
        log.info(f"Performing web search for prompt: '{sanitized_prompt}'")
        search_results = search.query_searxng(sanitized_prompt)

        if search_results:
            final_prompt = (
                "Based on the following real-time web search results, provide a comprehensive answer to the user's question.\n\n"
                "--- Web Search Results ---\n"
                f"{search_results}\n"
                "--- End of Search Results ---\n\n"
                f"User's Question: {sanitized_prompt}"
            )
            log.info("Constructed a new prompt with search results.")
        else:
            # Fallback if search fails or returns nothing
            log.warning(
                "Web search failed or returned no results. Using original prompt."
            )
            final_prompt = sanitized_prompt

        response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={"model": data.model, "prompt": final_prompt, "stream": False},
            timeout=60,
        )
        response.raise_for_status()

        ollama_data = response.json()
        model_response = ollama_data.get("response", "No response content from model.")

        # --- SAVE ORIGINAL PROMPT (not the big one) TO DB ---
        background_tasks.add_task(
            save_to_db_background, data.username, sanitized_prompt, model_response
        )

        return {"response": model_response}

    except requests.exceptions.RequestException as e:
        log.error(f"Error contacting Ollama: {e}")
        raise HTTPException(
            status_code=503, detail="Could not connect to the Ollama service."
        )
    except Exception as e:
        log.error(
            f"An unexpected error occurred in generate_prompt: {e}", exc_info=True
        )
        raise HTTPException(
            status_code=500, detail="An internal server error occurred."
        )


@app.on_event("startup")
async def startup_event():
    """On startup, set up the database."""
    vector_db.setup_database()


@app.get("/")
def read_root():
    return {"status": "Ollama API Wrapper is running"}


@app.get("/health")
def health_check():
    """Provides a simple health check endpoint."""
    return {"status": "ok"}
